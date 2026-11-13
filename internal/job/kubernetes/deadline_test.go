package kubernetes

import (
	"context"
	"net/http"
	"testing"
	"time"
)

const (
	stalledRequestTimeout = "200ms"
	stallTolerance        = 2 * time.Second
)

func TestOrchestrator_FailsFastWhenTheAPIServerStalls(t *testing.T) {
	for _, operation := range jobOperations {
		t.Run(operation.name, func(t *testing.T) {
			t.Setenv("KUBE_REQUEST_TIMEOUT", stalledRequestTimeout)
			o := newStalledOrchestrator(t, operation.targets, nil)

			began := time.Now()
			if err := awaitStalledOperation(t, operation.name, func() error { return operation.run(context.Background(), o) }); err == nil {
				t.Fatalf("%s against a stalled API server returned nil after %s, want an error", operation.name, time.Since(began))
			}
		})
	}
}

func newStalledOrchestrator(t *testing.T, stalls func(*http.Request) bool, reached chan<- struct{}) *Orchestrator {
	t.Helper()
	release := make(chan struct{})
	o := newStubbedOrchestrator(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !stalls(request) {
			serveStub(writer, request)
			return
		}
		if reached != nil {
			select {
			case reached <- struct{}{}:
			default:
			}
		}
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })
	return o
}

func awaitStalledOperation(t *testing.T, name string, run func() error) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- run() }()

	timer := time.NewTimer(stallTolerance)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatalf("%s against a stalled API server still blocked after %s, want an error once its deadline passes or its caller gives up",
			name, stallTolerance)
		return nil
	}
}
