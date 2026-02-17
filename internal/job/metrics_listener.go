package job

import (
	"context"
	"orchestrator/internal/observability"
)

// MetricsListener records job metrics from lifecycle events.
type MetricsListener struct {
	metrics *observability.Metrics
}

// NewMetricsListener creates a new MetricsListener.
func NewMetricsListener(metrics *observability.Metrics) *MetricsListener {
	return &MetricsListener{metrics: metrics}
}

// OnJobEvent records metrics for exit events.
func (l *MetricsListener) OnJobEvent(event *Event) {
	if l.metrics == nil || event.Payload == nil {
		return
	}

	if event.Payload.Type != EventTypeExit {
		return
	}

	data := event.Payload.Data

	image, _ := data["image"].(string)
	exitCode := -1
	if code, ok := data["exitCode"].(int); ok {
		exitCode = code
	}
	var duration float64
	if d, ok := data["durationSeconds"].(float64); ok {
		duration = d
	}

	l.metrics.RecordJobCompleted(context.Background(), image, exitCode == 0, duration)
}

// Verify MetricsListener implements EventListener
var _ EventListener = (*MetricsListener)(nil)
