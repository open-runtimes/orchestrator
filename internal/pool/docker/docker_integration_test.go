//go:build integration

package docker

import (
	"context"
	"errors"
	"fmt"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/pool"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

func sidecarTestImage() string {
	if img := os.Getenv("DEPLOYMENT_SIDECAR_IMAGE"); img != "" {
		return img
	}
	return "ko.local/deployments-sidecar:latest"
}

func shimTestImage() string {
	if img := os.Getenv("POOL_SHIM_IMAGE"); img != "" {
		return img
	}
	return "ko.local/pool-shim:latest"
}

// testPool returns an exec pool of the given size over alpine (which has
// /bin/sh — the shim execs the command via sh).
func testPool(name string, size int) pool.Pool {
	return pool.Pool{
		ID:    fmt.Sprintf("it-%s-%d", name, time.Now().UnixNano()%1e9),
		Image: "alpine:latest",
		Size:  size,
		CPU:   1, Memory: 128,
		Burst: pool.BurstReject,
	}
}

func newTestOrchestrator(t *testing.T, pools ...pool.Pool) *Orchestrator {
	t.Helper()

	cfg := LoadConfigFromEnv()
	cfg.SidecarImage = sidecarTestImage()
	cfg.ShimImage = shimTestImage()
	cfg.Pools = pools

	o, err := NewOrchestrator(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	t.Cleanup(func() {
		// Stop the replenishment loop before tearing slots down, or it
		// replaces them as fast as we delete them; keep the client for the
		// teardown itself.
		if o.loopCancel != nil {
			o.loopCancel()
			<-o.loopDone
		}
		ctx := context.Background()
		for _, p := range pools {
			views, err := o.slotsFor(ctx, p.ID)
			if err != nil {
				continue
			}
			for slotID := range views {
				o.removeSlot(ctx, p.ID, slotID)
			}
		}
		_ = o.client.Close()
	})

	if err := o.Start(t.Context()); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}
	return o
}

// waitWarm polls Pools() until the pool reports the wanted warm count.
func waitWarm(t *testing.T, o *Orchestrator, poolID string, want int) {
	t.Helper()

	testutil.MustWaitFor(t, func() bool {
		statuses, err := o.Pools(t.Context())
		if err != nil {
			return false
		}
		for _, s := range statuses {
			if s.ID == poolID {
				return s.Warm == want
			}
		}
		return false
	}, testutil.WithTimeout(120*time.Second), testutil.WithInterval(time.Second))
}

// warmSlotIDs returns the pool's unclaimed healthy slot IDs.
func warmSlotIDs(t *testing.T, o *Orchestrator, poolID string) map[string]bool {
	t.Helper()

	views, err := o.slotsFor(t.Context(), poolID)
	if err != nil {
		t.Fatalf("Failed to list slots: %v", err)
	}
	warm := make(map[string]bool)
	for slotID, s := range views {
		if s.sidecar == nil || s.sidecar.State != container.StateRunning {
			continue
		}
		if o.sidecarHealth(t.Context(), s.sidecar.ID) != container.Healthy {
			continue
		}
		cs, err := o.claimState(t.Context(), s.sidecar)
		if err != nil || cs.Claimed || cs.Failed {
			continue
		}
		warm[slotID] = true
	}
	return warm
}

func TestPool_ActivateExecAndReplenish(t *testing.T) {
	ctx := t.Context()
	p := testPool("exec", 1)
	o := newTestOrchestrator(t, p)

	waitWarm(t, o, p.ID, 1)
	before := warmSlotIDs(t, o, p.ID)

	status, err := o.Activate(ctx, p.ID, &pool.Activation{Command: "echo hello-pool && exit 0"})
	if err != nil {
		t.Fatalf("Failed to activate: %v", err)
	}
	if status.State != pool.StateExited {
		t.Fatalf("State = %s (error %q), want exited", status.State, status.Error)
	}
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", status.ExitCode)
	}
	if !strings.Contains(status.Output, "hello-pool") {
		t.Errorf("Output = %q, want it to contain hello-pool", status.Output)
	}

	// Status and List agree, derived from the exited workload container.
	got, err := o.Status(ctx, p.ID, status.ID)
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if got.State != pool.StateExited || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("Status = %s/%v, want exited/0", got.State, got.ExitCode)
	}
	list, err := o.List(ctx, p.ID)
	if err != nil {
		t.Fatalf("Failed to list activations: %v", err)
	}
	if len(list) != 1 || list[0].ID != status.ID {
		t.Errorf("List = %v, want the one activation %s", list, status.ID)
	}

	// The claimed slot is never reused: the loop replenishes warm capacity
	// with a NEW slot.
	waitWarm(t, o, p.ID, 1)
	for slotID := range warmSlotIDs(t, o, p.ID) {
		if before[slotID] {
			t.Errorf("Warm slot %s was reused; a claimed slot must be replaced", slotID)
		}
	}

	// Deactivate tears the slot down; the activation is gone.
	if err := o.Deactivate(ctx, p.ID, status.ID); err != nil {
		t.Fatalf("Failed to deactivate: %v", err)
	}
	if _, err := o.Status(ctx, p.ID, status.ID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("Status after deactivate = %v, want not found", err)
	}
}

func TestPool_ClaimRace(t *testing.T) {
	ctx := t.Context()
	p := testPool("race", 1)
	o := newTestOrchestrator(t, p)

	waitWarm(t, o, p.ID, 1)

	// Three concurrent activations race for the single warm slot: the sidecar
	// serializes (first claim wins, 409 for the rest), and with burst=reject
	// the losers map to exhausted (429).
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = o.Activate(ctx, p.ID, &pool.Activation{Command: "sleep 2 && exit 0"})
		})
	}
	wg.Wait()

	won, exhausted := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, apperrors.ErrExhausted):
			exhausted++
		default:
			t.Errorf("Unexpected activation error: %v", err)
		}
	}
	if won != 1 || exhausted != 2 {
		t.Errorf("Race outcome = %d wins, %d exhausted, want 1 and 2", won, exhausted)
	}
}

func TestPool_NonZeroExitPropagates(t *testing.T) {
	ctx := t.Context()
	p := testPool("exit", 1)
	o := newTestOrchestrator(t, p)

	waitWarm(t, o, p.ID, 1)

	status, err := o.Activate(ctx, p.ID, &pool.Activation{Command: "echo boom >&2; exit 7"})
	if err != nil {
		t.Fatalf("Failed to activate: %v", err)
	}
	if status.State != pool.StateExited {
		t.Fatalf("State = %s (error %q), want exited", status.State, status.Error)
	}
	if status.ExitCode == nil || *status.ExitCode != 7 {
		t.Errorf("ExitCode = %v, want 7", status.ExitCode)
	}
	if !strings.Contains(status.Output, "boom") {
		t.Errorf("Output = %q, want it to contain boom (stderr is captured)", status.Output)
	}
}

func TestPool_HTTPPoolRejectedOnDocker(t *testing.T) {
	p := testPool("http", 1)
	p.Port = 8080
	o := newTestOrchestrator(t) // not configured — construct directly to skip warming
	o.pools[p.ID] = p

	if _, err := o.Activate(t.Context(), p.ID, &pool.Activation{Command: "true"}); !errors.Is(err, apperrors.ErrValidation) {
		t.Errorf("Activate(HTTP pool) = %v, want validation error", err)
	}
}
