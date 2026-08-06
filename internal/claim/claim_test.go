package claim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/workload"
	"strconv"
	"strings"
	"testing"
)

// fakeInventory yields a fixed free set; Create mints cold-N units.
type fakeInventory struct {
	free    []Unit
	freeErr error

	coldErr   error
	coldCalls int
}

func (f *fakeInventory) Free(context.Context) ([]Unit, error) {
	return f.free, f.freeErr
}

func (f *fakeInventory) Create(context.Context) (*Unit, error) {
	f.coldCalls++
	if f.coldErr != nil {
		return nil, f.coldErr
	}
	return &Unit{ID: "cold-" + strconv.Itoa(f.coldCalls), Addr: "10.0.0.99", Token: "t"}, nil
}

// fakePoster scripts each unit's claim outcome by unit ID; unscripted units
// accept.
type fakePoster struct {
	outcomes map[string]error
	posted   []string
}

func (f *fakePoster) Post(_ context.Context, u Unit, _ *workload.ClaimRequest) error {
	f.posted = append(f.posted, u.ID)
	return f.outcomes[u.ID]
}

func unit(id string) Unit { return Unit{ID: id, Addr: "10.0.0.1", Token: "t"} }

func req() *workload.ClaimRequest { return &workload.ClaimRequest{ActivationID: "act"} }

func TestClaimWinsFirstFreeUnit(t *testing.T) {
	inv := &fakeInventory{free: []Unit{unit("a"), unit("b")}}
	post := &fakePoster{}

	won, err := Claim(t.Context(), inv, post, nil, "p", BurstReject, req())
	if err != nil {
		t.Fatal(err)
	}
	if won.ID != "a" || len(post.posted) != 1 {
		t.Errorf("won %q after %v, want a after one post", won.ID, post.posted)
	}
}

func TestClaimRetriesNextUnitOnConflict(t *testing.T) {
	inv := &fakeInventory{free: []Unit{unit("a"), unit("b"), unit("c")}}
	post := &fakePoster{outcomes: map[string]error{"a": ErrConflict, "b": ErrConflict}}

	won, err := Claim(t.Context(), inv, post, nil, "p", BurstReject, req())
	if err != nil || won.ID != "c" {
		t.Fatalf("won %v (%v), want c — racing losers must try the next unit", won, err)
	}
}

func TestClaimSkipsTransientFailures(t *testing.T) {
	inv := &fakeInventory{free: []Unit{unit("broken"), unit("b")}}
	post := &fakePoster{outcomes: map[string]error{"broken": errors.New("connection refused")}}

	won, err := Claim(t.Context(), inv, post, nil, "p", BurstReject, req())
	if err != nil || won.ID != "b" {
		t.Fatalf("won %v (%v), want b — one broken unit must not fail the activation", won, err)
	}
}

func TestClaimPoisonStopsAndStampsUnit(t *testing.T) {
	inv := &fakeInventory{free: []Unit{unit("a"), unit("b")}}
	post := &fakePoster{outcomes: map[string]error{"a": &Poison{Msg: "artifacts failed"}}}

	_, err := Claim(t.Context(), inv, post, nil, "p", BurstReject, req())
	var poison *Poison
	if !errors.As(err, &poison) {
		t.Fatalf("got %v, want Poison — the sidecar accepted, the activation is spent", err)
	}
	if poison.Unit != "a" {
		t.Errorf("poison stamped %q, want a", poison.Unit)
	}
	if len(post.posted) != 1 {
		t.Errorf("posted to %v after poison, want no further units", post.posted)
	}
}

func TestClaimExhaustedRejectsWithoutColdCreate(t *testing.T) {
	inv := &fakeInventory{}
	post := &fakePoster{}

	_, err := Claim(t.Context(), inv, post, nil, "p", BurstReject, req())
	if !errors.Is(err, apperrors.ErrExhausted) {
		t.Fatalf("got %v, want Exhausted", err)
	}
	if inv.coldCalls != 0 {
		t.Errorf("cold-created %d units under burst=reject, want 0", inv.coldCalls)
	}
}

func TestClaimBurstColdCreatesAndClaims(t *testing.T) {
	inv := &fakeInventory{}
	post := &fakePoster{}

	won, err := Claim(t.Context(), inv, post, nil, "p", BurstCold, req())
	if err != nil || won.ID != "cold-1" {
		t.Fatalf("won %v (%v), want cold-1", won, err)
	}
}

func TestClaimStolenColdUnitRetriesWarmPass(t *testing.T) {
	inv := &fakeInventory{}
	post := &fakePoster{outcomes: map[string]error{"cold-1": ErrConflict}}
	// After the cold unit is stolen, a warm unit has appeared (the thief's
	// pool replenished, or another slot freed).
	inv.free = []Unit{unit("late")}

	won, err := Claim(t.Context(), inv, post, nil, "p", BurstCold, req())
	if err != nil || won.ID != "late" {
		t.Fatalf("won %v (%v), want late via the post-steal warm pass", won, err)
	}
}

func TestClaimStolenColdUnitExhaustsWhenNothingFrees(t *testing.T) {
	inv := &fakeInventory{}
	post := &fakePoster{outcomes: map[string]error{"cold-1": ErrConflict}}

	_, err := Claim(t.Context(), inv, post, nil, "p", BurstCold, req())
	if !errors.Is(err, apperrors.ErrExhausted) {
		t.Fatalf("got %v, want Exhausted after the stolen cold unit", err)
	}
}

func TestHTTPPosterMapsProtocolStatuses(t *testing.T) {
	var status int
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != workload.ClaimPath {
			t.Errorf("posted to %s, want %s", r.URL.Path, workload.ClaimPath)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("because"))
	}))
	defer server.Close()

	// The poster addresses Addr on the fixed admin port; point it at the test
	// server instead by rewriting through its URL.
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	poster := &httpPoster{client: server.Client()}
	target := Unit{ID: "u", Addr: u.Hostname(), Token: "tok"}
	post := func() error {
		return poster.postTo(t.Context(), server.URL+workload.ClaimPath, target, req())
	}

	status = http.StatusOK
	if err := post(); err != nil {
		t.Errorf("200: got %v, want nil", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want the unit's bearer token", gotAuth)
	}

	status = http.StatusConflict
	if err := post(); !errors.Is(err, ErrConflict) {
		t.Errorf("409: got %v, want ErrConflict", err)
	}

	status = http.StatusUnprocessableEntity
	var poison *Poison
	if err := post(); !errors.As(err, &poison) || !strings.Contains(poison.Msg, "because") {
		t.Errorf("422: got %v, want Poison carrying the sidecar's message", err)
	}

	status = http.StatusInternalServerError
	if err := post(); err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("500: got %v, want a plain error", err)
	}
}

// countRecorder captures claim protocol metric calls.
type countRecorder struct {
	conflicts, poisons int
	bursts             []string
}

func (c *countRecorder) RecordPoolConflict(context.Context, string) { c.conflicts++ }
func (c *countRecorder) RecordPoolPoisoned(context.Context, string) { c.poisons++ }
func (c *countRecorder) RecordPoolBurst(_ context.Context, _, policy string) {
	c.bursts = append(c.bursts, policy)
}

func TestClaimRecordsTelemetry(t *testing.T) {
	// Conflict on a, poison on b: one conflict, one poison, no burst.
	rec := &countRecorder{}
	inv := &fakeInventory{free: []Unit{unit("a"), unit("b")}}
	post := &fakePoster{outcomes: map[string]error{"a": ErrConflict, "b": &Poison{Msg: "boom"}}}
	if _, err := Claim(t.Context(), inv, post, rec, "p", BurstReject, req()); err == nil {
		t.Fatal("want poison error")
	}
	if rec.conflicts != 1 || rec.poisons != 1 || len(rec.bursts) != 0 {
		t.Errorf("got conflicts=%d poisons=%d bursts=%v, want 1/1/[]", rec.conflicts, rec.poisons, rec.bursts)
	}

	// Empty pool, burst=reject.
	rec = &countRecorder{}
	if _, err := Claim(t.Context(), &fakeInventory{}, &fakePoster{}, rec, "p", BurstReject, req()); err == nil {
		t.Fatal("want exhausted")
	}
	if len(rec.bursts) != 1 || rec.bursts[0] != BurstReject {
		t.Errorf("bursts = %v, want [reject]", rec.bursts)
	}

	// Empty pool, burst=cold.
	rec = &countRecorder{}
	if _, err := Claim(t.Context(), &fakeInventory{}, &fakePoster{}, rec, "p", BurstCold, req()); err != nil {
		t.Fatalf("cold create: %v", err)
	}
	if len(rec.bursts) != 1 || rec.bursts[0] != BurstCold {
		t.Errorf("bursts = %v, want [cold]", rec.bursts)
	}
}
