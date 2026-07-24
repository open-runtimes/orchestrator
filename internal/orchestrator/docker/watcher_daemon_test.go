package docker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"orchestrator/pkg/job"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
)

// fakeDaemon fakes the subset of the Docker Engine API the lifecycle watcher
// touches (events, inspect, start, kill, logs), so watcher state transitions
// — including OOM classification and reconnects — can be driven event by
// event without a daemon.
type fakeDaemon struct {
	mu      sync.Mutex
	inspect map[string]string // container id -> inspect JSON body

	conns   int
	stream  func(conn int, send func(action, id string, attrs map[string]string), done <-chan struct{})
	onStart func() // called when the worker container is started
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{inspect: make(map[string]string)}
}

func (d *fakeDaemon) setInspect(id, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inspect[id] = body
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/events"):
		d.mu.Lock()
		d.conns++
		conn := d.conns
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		send := func(action, id string, attrs map[string]string) {
			msg := events.Message{
				Type:   events.ContainerEventType,
				Action: events.Action(action),
				Actor:  events.Actor{ID: id, Attributes: attrs},
			}
			b, _ := json.Marshal(msg)
			_, _ = w.Write(b)
			fl.Flush()
		}
		d.stream(conn, send, r.Context().Done())

	case strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/json"):
		parts := strings.Split(path, "/")
		id := parts[len(parts)-2]
		d.mu.Lock()
		body, ok := d.inspect[id]
		d.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))

	case strings.HasSuffix(path, "/start"):
		if d.onStart != nil {
			d.onStart()
		}
		w.WriteHeader(http.StatusNoContent)

	case strings.HasSuffix(path, "/kill"):
		w.WriteHeader(http.StatusNoContent)

	case strings.HasSuffix(path, "/logs"):
		w.WriteHeader(http.StatusOK) // empty stream; EOF ends log streaming

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

const sidecarHealthyJSON = `{"Id":"sc-1","State":{"Running":true,"Status":"running","Health":{"Status":"healthy"}}}`

func workerJSON(status string, running bool, exitCode int, oomKilled bool) string {
	return fmt.Sprintf(
		`{"Id":"wk-1","State":{"Running":%v,"Status":%q,"ExitCode":%d,"OOMKilled":%v,"StartedAt":"2024-01-01T00:00:00Z"}}`,
		running, status, exitCode, oomKilled,
	)
}

// newFakeDaemonWatcher wires a dockerLifecycleWatcher to a fakeDaemon. The
// worker starts as "created" so the watcher itself starts it via the healthy
// sidecar, mirroring the real flow.
func newFakeDaemonWatcher(t *testing.T, d *fakeDaemon) (*dockerLifecycleWatcher, chan struct{}) {
	t.Helper()
	d.setInspect("sc-1", sidecarHealthyJSON)
	d.setInspect("wk-1", workerJSON("created", false, 0, false))
	started := make(chan struct{})
	d.onStart = func() {
		d.setInspect("wk-1", workerJSON("running", true, 0, false))
		close(started)
	}

	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")),
		client.WithHTTPClient(srv.Client()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	return newDockerLifecycleWatcher(cli), started
}

// watchAndCollect runs Watch to completion and returns the emitted signals,
// with log lines filtered out.
func watchAndCollect(t *testing.T, w *dockerLifecycleWatcher) []job.Signal {
	t.Helper()
	var mu sync.Mutex
	var sigs []job.Signal
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
			if _, ok := s.(job.LogLine); ok {
				return
			}
			mu.Lock()
			sigs = append(sigs, s)
			mu.Unlock()
		})
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("watcher did not finish")
	}
	return sigs
}

func assertLifecycleWithExit(t *testing.T, sigs []job.Signal, wantCode int, wantReason string) {
	t.Helper()
	if len(sigs) != 3 {
		t.Fatalf("want [Started, Exited, Completed], got %v", sigs)
	}
	if _, ok := sigs[0].(job.Started); !ok {
		t.Errorf("signal[0]: want Started, got %T", sigs[0])
	}
	exited, ok := sigs[1].(job.Exited)
	if !ok {
		t.Fatalf("signal[1]: want Exited, got %T", sigs[1])
	}
	if exited.ExitCode != wantCode || exited.Reason != wantReason {
		t.Errorf("want Exited{ExitCode: %d, Reason: %q}, got Exited{ExitCode: %d, Reason: %q}",
			wantCode, wantReason, exited.ExitCode, exited.Reason)
	}
	if _, ok := sigs[2].(job.Completed); !ok {
		t.Errorf("signal[2]: want Completed, got %T", sigs[2])
	}
}

// The oom event precedes die, and the classification must come from the
// tracked event: the exited worker's inspect deliberately reports
// OOMKilled=false (a cgroup v1 daemon that only flags pid 1).
func TestWatcher_OOMEventThenDie_EmitsOOMReason(t *testing.T) {
	t.Parallel()
	d := newFakeDaemon()
	w, started := newFakeDaemonWatcher(t, d)

	d.stream = func(conn int, send func(action, id string, attrs map[string]string), done <-chan struct{}) {
		<-started
		send("oom", "wk-1", nil)
		d.setInspect("wk-1", workerJSON("exited", false, 137, false))
		send("die", "wk-1", map[string]string{"exitCode": "137"})
		send("die", "sc-1", nil)
		<-done
	}

	assertLifecycleWithExit(t, watchAndCollect(t, w), 137, job.ExitReasonOOM)
}

// The oom event was missed (e.g. emitted while the stream was down), so the
// die handler must fall back to the exited worker's OOMKilled inspect flag.
func TestWatcher_DieWithOOMKilledInspect_EmitsOOMReason(t *testing.T) {
	t.Parallel()
	d := newFakeDaemon()
	w, started := newFakeDaemonWatcher(t, d)

	d.stream = func(conn int, send func(action, id string, attrs map[string]string), done <-chan struct{}) {
		<-started
		d.setInspect("wk-1", workerJSON("exited", false, 137, true))
		send("die", "wk-1", map[string]string{"exitCode": "137"})
		send("die", "sc-1", nil)
		<-done
	}

	assertLifecycleWithExit(t, watchAndCollect(t, w), 137, job.ExitReasonOOM)
}

// A SIGKILL without any OOM evidence must NOT be classified as OOM.
func TestWatcher_DieWithoutOOM_EmitsNoReason(t *testing.T) {
	t.Parallel()
	d := newFakeDaemon()
	w, started := newFakeDaemonWatcher(t, d)

	d.stream = func(conn int, send func(action, id string, attrs map[string]string), done <-chan struct{}) {
		<-started
		d.setInspect("wk-1", workerJSON("exited", false, 137, false))
		send("die", "wk-1", map[string]string{"exitCode": "137"})
		send("die", "sc-1", nil)
		<-done
	}

	assertLifecycleWithExit(t, watchAndCollect(t, w), 137, "")
}

// The oom event arrives on the first stream, which then drops before die.
// The worker's exit is detected by reconcile on the reconnected stream — with
// inspect again reporting OOMKilled=false, only oom state carried across the
// reconnect can classify the exit.
func TestWatcher_OOMBeforeReconnect_ReasonSurvivesReconcile(t *testing.T) {
	t.Parallel()
	d := newFakeDaemon()
	w, started := newFakeDaemonWatcher(t, d)

	d.stream = func(conn int, send func(action, id string, attrs map[string]string), done <-chan struct{}) {
		if conn == 1 {
			<-started
			send("oom", "wk-1", nil)
			// The worker dies while the stream is down; the reconnected
			// watcher finds it already exited during reconcile.
			d.setInspect("wk-1", workerJSON("exited", false, 137, false))
			return // disconnect
		}
		send("die", "sc-1", nil)
		<-done
	}

	assertLifecycleWithExit(t, watchAndCollect(t, w), 137, job.ExitReasonOOM)
}
