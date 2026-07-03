package kube

import (
	"context"
	"net/http"
	"orchestrator/internal/observability"
	"strings"
	"time"
)

// newMetricsTransport returns a rest.Config.Wrap-compatible transport wrapper
// that records K8s API request latency and error counters.
func newMetricsTransport(metrics *observability.Metrics) func(http.RoundTripper) http.RoundTripper {
	return func(rt http.RoundTripper) http.RoundTripper {
		return &metricsTransport{inner: rt, metrics: metrics}
	}
}

type metricsTransport struct {
	inner   http.RoundTripper
	metrics *observability.Metrics
}

func (m *metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := m.inner.RoundTrip(req)
	dur := time.Since(start).Seconds()

	ctx := req.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	verb := req.Method
	resource := parseResourceFromPath(req.URL.Path)

	status := 0
	switch {
	case err != nil:
		status = -1 // transport-level failure
	case resp != nil:
		status = resp.StatusCode
	}
	m.metrics.RecordK8sAPIRequest(ctx, verb, resource, dur, status)
	return resp, err
}

// parseResourceFromPath extracts the resource name from a K8s API path. We
// only need enough fidelity to give operators a useful label — the full
// verb/subresource breakdown isn't worth the cardinality.
//
// Supported shapes:
//
//	/api/v1/namespaces/<ns>/<resource>[/<name>[/subresource]]
//	/apis/<group>/<version>/namespaces/<ns>/<resource>[/<name>]
//	/api/v1/<cluster-scoped-resource>[/<name>]
func parseResourceFromPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := range parts {
		if parts[i] == "namespaces" && i+2 < len(parts) {
			return parts[i+2]
		}
	}
	// Cluster-scoped: last non-empty segment is the resource when there's no
	// trailing name.
	if len(parts) >= 3 {
		return parts[2]
	}
	return "unknown"
}
