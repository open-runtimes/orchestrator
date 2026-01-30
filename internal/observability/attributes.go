// Package observability provides metrics, tracing, and logging utilities.
package observability

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Attribute keys
const (
	attrMethod  = "method"
	attrPath    = "path"
	attrStatus  = "status"
	attrImage   = "image"
	attrSuccess = "success"
)

func methodAttr(method string) attribute.KeyValue {
	return attribute.String(attrMethod, method)
}

func pathAttr(path string) attribute.KeyValue {
	// Normalize paths with IDs to reduce cardinality
	// /v1/jobs/abc123 -> /v1/jobs/{jobId}
	normalized := normalizePath(path)
	return attribute.String(attrPath, normalized)
}

func statusAttr(code int) attribute.KeyValue {
	// Group status codes to reduce cardinality
	// 200-299 -> 2xx, 400-499 -> 4xx, 500-599 -> 5xx
	group := fmt.Sprintf("%dxx", code/100)
	return attribute.String(attrStatus, group)
}

func imageAttr(image string) attribute.KeyValue {
	return attribute.String(attrImage, image)
}

func successAttr(success bool) attribute.KeyValue {
	return attribute.Bool(attrSuccess, success)
}

// normalizePath replaces dynamic path segments with placeholders.
func normalizePath(path string) string {
	// Simple normalization for /v1/jobs/{jobId}
	// More sophisticated routing-aware normalization would be better
	const prefix = "/v1/jobs/"
	if len(path) > len(prefix) && path[:len(prefix)] == prefix {
		return "/v1/jobs/{jobId}"
	}
	return path
}

// WithMethod returns a metric option with the method attribute.
func WithMethod(method string) metric.MeasurementOption {
	return metric.WithAttributes(methodAttr(method))
}

// WithPath returns a metric option with the path attribute.
func WithPath(path string) metric.MeasurementOption {
	return metric.WithAttributes(pathAttr(path))
}

// WithStatus returns a metric option with the status attribute.
func WithStatus(code int) metric.MeasurementOption {
	return metric.WithAttributes(statusAttr(code))
}

// WithImage returns a metric option with the image attribute.
func WithImage(image string) metric.MeasurementOption {
	return metric.WithAttributes(imageAttr(image))
}

// WithSuccess returns a metric option with the success attribute.
func WithSuccess(success bool) metric.MeasurementOption {
	return metric.WithAttributes(successAttr(success))
}
