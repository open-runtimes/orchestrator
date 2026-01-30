// Package cloudevent provides CloudEvents 1.0 types.
package cloudevent

import "time"

// CloudEvent represents a CloudEvents 1.0 specification event
type CloudEvent struct {
	SpecVersion     string         `json:"specversion"`
	Type            string         `json:"type"`
	Source          string         `json:"source"`
	Subject         string         `json:"subject"`
	ID              string         `json:"id"`
	Time            time.Time      `json:"time"`
	DataContentType string         `json:"datacontenttype"`
	Data            map[string]any `json:"data"`
}

// New creates a new CloudEvent with default values
func New(eventType, source, subject, id string, data map[string]any) *CloudEvent {
	return &CloudEvent{
		SpecVersion:     "1.0",
		Type:            eventType,
		Source:          source,
		Subject:         subject,
		ID:              id,
		Time:            time.Now().UTC(),
		DataContentType: "application/json",
		Data:            data,
	}
}
