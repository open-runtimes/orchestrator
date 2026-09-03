// Package claim owns the warm-unit claim protocol shared by every consumer of
// standing warm capacity — deployment Revisions and sandboxes, including cold
// starts: iterate the free warm units, atomically reserve one in the backend,
// then activate it through its sidecar. Reserving before activation is the
// metadata barrier: final workload identity is durable before user code can
// emit output. Backends stay inventories: how to list, reserve, create, and
// discard warm units.
package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/workload"
	"strconv"
	"time"
)

// Burst policies: what happens when a claim arrives and no warm unit is
// free. Declared here because the flow implements them; fleet declarations
// validate against these names.
const (
	BurstReject = "reject" // 429 — never pay a cold start on the request path
	BurstCold   = "cold"   // create a unit on demand and pay the cold start
)

// ErrConflict is the racing loser's result: the backend reservation changed or
// the unit's sidecar already accepted another claim. The flow retries another.
var ErrConflict = errors.New("unit already claimed")

// Poison is the 422 claim outcome: the sidecar accepted the claim but
// artifact materialization failed — the unit is poisoned (never resold) and
// the claim has failed.
//
//nolint:errname // single-word names; the package is the namespace
type Poison struct {
	Unit string // stamped by the flow with the poisoned unit
	Msg  string
}

func (e *Poison) Error() string { return e.Msg }

// Unit is one claimable warm unit an Inventory yields: a pod on Kubernetes,
// a slot on Docker.
type Unit struct {
	ID    string // pod name / slot ID
	Addr  string // sidecar admin address (IP; port is workload.DefaultAdminPort)
	Token string // claim bearer token
}

// Inventory is the backend's warm-unit surface behind the flow's seam.
type Inventory interface {
	// Free lists the currently claimable warm units, in claim order.
	Free(ctx context.Context) ([]Unit, error)
	// Create provisions one unit and returns it claimable — the burst
	// cold start. Implementations own the warm-up wait and discard units
	// that never turn claimable.
	Create(ctx context.Context) (*Unit, error)
	// Reserve atomically wins a free unit and stamps its final identity.
	// ErrConflict means another claimant changed the unit first.
	Reserve(ctx context.Context, unit Unit) error
	// Discard removes a reserved unit after activation fails or is uncertain.
	Discard(ctx context.Context, unit Unit) error
}

// Recorder receives the claim protocol's metrics. Satisfied by
// *observability.Metrics; nil disables recording.
type Recorder interface {
	RecordPoolConflict(ctx context.Context, id string)
	RecordPoolPoisoned(ctx context.Context, id string)
	RecordPoolBurst(ctx context.Context, id, policy string)
}

// Poster performs the sidecar claim POST. Faked in backend unit tests where
// pods have no reachable sidecars; production uses NewPoster.
type Poster interface {
	Post(ctx context.Context, u Unit, req *workload.ClaimRequest) error
}

// Claim wins one warm unit for the request. With no free unit the pool's
// burst policy decides: reject (429-mapped) or cold-create. Returns
// *Poison when the winning unit's artifacts failed — the claim is
// failed, not errored.
func Claim(ctx context.Context, inv Inventory, post Poster, rec Recorder, poolID, burst string, req *workload.ClaimRequest) (*Unit, error) {
	unit, ok, err := tryWarm(ctx, inv, post, rec, poolID, req)
	if err != nil || ok {
		return unit, recordPoison(ctx, rec, poolID, err)
	}

	if burst != BurstCold {
		if rec != nil {
			rec.RecordPoolBurst(ctx, poolID, BurstReject)
		}
		slog.Warn("Pool exhausted, rejecting claim", "poolId", poolID, "claimId", req.ClaimID)
		return nil, exhausted(poolID)
	}

	if rec != nil {
		rec.RecordPoolBurst(ctx, poolID, BurstCold)
	}
	slog.Warn("Pool exhausted, cold-creating capacity", "poolId", poolID, "claimId", req.ClaimID)
	created, err := inv.Create(ctx)
	if err != nil {
		return nil, err
	}
	err = inv.Reserve(ctx, *created)
	if errors.Is(err, ErrConflict) {
		if rec != nil {
			rec.RecordPoolConflict(ctx, poolID)
		}
		if unit, ok, retryErr := tryWarm(ctx, inv, post, rec, poolID, req); retryErr != nil || ok {
			return unit, recordPoison(ctx, rec, poolID, retryErr)
		}
		return nil, exhausted(poolID)
	}
	if err != nil {
		return nil, reservationFailure(ctx, inv, *created, err)
	}
	err = postReserved(ctx, inv, post, *created, req)
	switch {
	case err == nil:
		return created, nil
	case errors.Is(err, ErrConflict):
		// A sidecar conflict after our metadata reservation means a claimant
		// reached an older pod before this protocol. The unit is already
		// discarded; make one more warm pass.
		if rec != nil {
			rec.RecordPoolConflict(ctx, poolID)
		}
		if unit, ok, err := tryWarm(ctx, inv, post, rec, poolID, req); err != nil || ok {
			return unit, recordPoison(ctx, rec, poolID, err)
		}
		return nil, exhausted(poolID)
	default:
		return nil, recordPoison(ctx, rec, poolID, Outcome(err, created.ID))
	}
}

// recordPoison bumps the poison counter when err is a *Poison, passing err
// through either way.
func recordPoison(ctx context.Context, rec Recorder, poolID string, err error) error {
	var poison *Poison
	if rec != nil && errors.As(err, &poison) {
		rec.RecordPoolPoisoned(ctx, poolID)
	}
	return err
}

// tryWarm reserves and activates the first free unit that accepts; ok=false
// with nil error means none was free. Reservation conflicts move to the next
// unit. Activation failures discard the reserved unit before the flow retries;
// poison stops the pass because the requested workload itself failed.
func tryWarm(ctx context.Context, inv Inventory, post Poster, rec Recorder, poolID string, req *workload.ClaimRequest) (*Unit, bool, error) {
	units, err := inv.Free(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, u := range units {
		err := inv.Reserve(ctx, u)
		if errors.Is(err, ErrConflict) {
			if rec != nil {
				rec.RecordPoolConflict(ctx, poolID)
			}
			continue
		}
		if err != nil {
			return nil, false, reservationFailure(ctx, inv, u, err)
		}
		err = postReserved(ctx, inv, post, u, req)
		switch {
		case err == nil:
			return &u, true, nil
		case errors.Is(err, ErrConflict):
			if rec != nil {
				rec.RecordPoolConflict(ctx, poolID)
			}
			continue // racing loser — try the next warm unit
		default:
			var poison *Poison
			if errors.As(err, &poison) {
				poison.Unit = u.ID
				return nil, true, poison
			}
			slog.Warn("Claim attempt failed", "poolId", poolID, "unit", u.ID, "error", err)
		}
	}
	return nil, false, nil
}

// postReserved activates a unit whose final metadata is already durable. Any
// non-success has an ambiguous start outcome, so the unit is discarded before
// the flow tries elsewhere or reports failure.
func postReserved(ctx context.Context, inv Inventory, post Poster, unit Unit, req *workload.ClaimRequest) error {
	err := post.Post(ctx, unit, req)
	if err == nil {
		return nil
	}
	if discardErr := inv.Discard(ctx, unit); discardErr != nil {
		return fmt.Errorf("claim failed: %v; discard reserved unit %s: %w", err, unit.ID, discardErr)
	}
	return err
}

func reservationFailure(ctx context.Context, inv Inventory, unit Unit, reserveErr error) error {
	if discardErr := inv.Discard(ctx, unit); discardErr != nil {
		return fmt.Errorf("reserve unit %s failed: %v; discard after ambiguous reservation: %w", unit.ID, reserveErr, discardErr)
	}
	return reserveErr
}

func exhausted(poolID string) error {
	return apperrors.Exhausted("pool", "pool "+poolID+" has no free warm capacity")
}

// Outcome maps a failed claim POST onto the protocol's vocabulary, stamping
// the unit onto a Poison so the caller knows which one to discard. Exported for
// the consumer that creates a unit for one request and claims it directly: with
// nothing standing there is no warm pass to fall back to, so the POST's result
// is the whole outcome.
func Outcome(err error, unitID string) error {
	var poison *Poison
	if errors.As(err, &poison) {
		poison.Unit = unitID
		return poison
	}
	return apperrors.Internal("pool.claim", err)
}

// httpPoster is the production Poster: it POSTs the claim to the unit's
// sidecar admin port with the unit's bearer token.
type httpPoster struct {
	client *http.Client
}

// NewPoster creates the production Poster with a bounded per-claim timeout.
// The seam is real — backend tests substitute fakes — so the concrete type
// stays hidden.
//
//nolint:iface // see above
func NewPoster() Poster {
	return &httpPoster{client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *httpPoster) Post(ctx context.Context, u Unit, req *workload.ClaimRequest) error {
	url := "http://" + net.JoinHostPort(u.Addr, strconv.Itoa(workload.DefaultAdminPort)) + workload.ClaimPath
	return p.postTo(ctx, url, u, req)
}

func (p *httpPoster) postTo(ctx context.Context, url string, u Unit, req *workload.ClaimRequest) error {
	// ClaimRequest's custom codec routes artifacts through the registry so
	// each carries its "type" discriminator.
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+u.Token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrConflict
	case http.StatusUnprocessableEntity:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &Poison{Msg: "artifacts failed: " + string(bytes.TrimSpace(msg))}
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("claim rejected with status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
}
