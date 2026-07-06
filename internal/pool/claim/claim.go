// Package claim owns the warm-pod claim protocol shared by the pool
// backends: iterate the free warm units, win one via the sidecar claim POST
// (the POST is the claim — the sidecar is the serialization point), fall to
// the pool's burst policy on exhaustion, and retry once when a cold-created
// unit is stolen by a racing activation. Backends stay inventories: how to
// list, create, and address warm units.
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
	"orchestrator/internal/proxy"
	"orchestrator/pkg/pool"
	"strconv"
	"time"
)

// ErrConflict is the racing loser's result: the unit's sidecar already
// accepted another activation. The flow retries the next warm unit.
var ErrConflict = errors.New("unit already claimed")

// Poison is the 422 claim outcome: the sidecar accepted the claim but
// artifact materialization failed — the unit is poisoned (never resold) and
// the activation has failed.
//
//nolint:errname // single-word names; the package is the namespace
type Poison struct {
	Unit string // stamped by the flow with the poisoned unit
	Msg    string
}

func (e *Poison) Error() string { return e.Msg }

// Unit is one claimable warm unit an Inventory yields: a pod on Kubernetes,
// a slot on Docker.
type Unit struct {
	ID    string // pod name / slot ID
	Addr  string // sidecar admin address (IP; port is proxy.DefaultAdminPort)
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
}

// Poster performs the sidecar claim POST. Faked in backend unit tests where
// pods have no reachable sidecars; production uses NewPoster.
type Poster interface {
	Post(ctx context.Context, u Unit, req *proxy.ClaimRequest) error
}

// Claim wins one warm unit for the request. With no free unit the pool's
// burst policy decides: reject (429-mapped) or cold-create. Returns
// *Poison when the winning unit's artifacts failed — the activation is
// failed, not errored.
func Claim(ctx context.Context, inv Inventory, post Poster, poolID, burst string, req *proxy.ClaimRequest) (*Unit, error) {
	unit, ok, err := tryWarm(ctx, inv, post, poolID, req)
	if err != nil || ok {
		return unit, err
	}

	if burst != pool.BurstCold {
		slog.Warn("Pool exhausted, rejecting activation", "poolId", poolID, "activationId", req.ActivationID)
		return nil, exhausted(poolID)
	}

	slog.Warn("Pool exhausted, cold-creating capacity", "poolId", poolID, "activationId", req.ActivationID)
	created, err := inv.Create(ctx)
	if err != nil {
		return nil, err
	}
	err = post.Post(ctx, *created, req)
	switch {
	case err == nil:
		return created, nil
	case errors.Is(err, ErrConflict):
		// The cold unit was stolen by a racing activation; one more warm pass.
		if unit, ok, err := tryWarm(ctx, inv, post, poolID, req); err != nil || ok {
			return unit, err
		}
		return nil, exhausted(poolID)
	default:
		return nil, poisonedOrInternal(err, created.ID)
	}
}

// tryWarm claims the first free unit that accepts; ok=false with nil error
// means none was free. Conflicts move to the next unit; transient claim
// failures are logged and skipped — one broken unit must not fail an
// activation while others are free. Poison stops the pass: the sidecar
// accepted, so the activation is spent.
func tryWarm(ctx context.Context, inv Inventory, post Poster, poolID string, req *proxy.ClaimRequest) (*Unit, bool, error) {
	units, err := inv.Free(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, u := range units {
		err := post.Post(ctx, u, req)
		switch {
		case err == nil:
			return &u, true, nil
		case errors.Is(err, ErrConflict):
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

func exhausted(poolID string) error {
	return apperrors.Exhausted("pool", "pool "+poolID+" has no free warm capacity")
}

func poisonedOrInternal(err error, unitID string) error {
	var poison *Poison
	if errors.As(err, &poison) {
		poison.Unit = unitID
		return poison
	}
	return apperrors.Internal("pool.claim", err)
}

// httpPoster is the production Poster: it POSTs the activation to the unit's
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

func (p *httpPoster) Post(ctx context.Context, u Unit, req *proxy.ClaimRequest) error {
	url := "http://" + net.JoinHostPort(u.Addr, strconv.Itoa(proxy.DefaultAdminPort)) + proxy.ClaimPath
	return p.postTo(ctx, url, u, req)
}

func (p *httpPoster) postTo(ctx context.Context, url string, u Unit, req *proxy.ClaimRequest) error {
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
