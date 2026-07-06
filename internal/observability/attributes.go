// Package observability provides metrics, tracing, and logging utilities.
package observability

import (
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Attribute keys
const (
	attrMethod     = "method"
	attrPath       = "path"
	attrStatus     = "status"
	attrImage      = "image"
	attrSuccess    = "success"
	attrIdentity   = "identity"
	attrVerb       = "verb"
	attrResource   = "resource"
	attrCreated    = "created"
	attrOutcome    = "outcome"
	attrResult     = "result"
	attrDirection  = "direction"
	attrDeployment = "deployment"
	attrPool       = "pool"
	attrPolicy     = "policy"
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

func identityAttr(identity string) attribute.KeyValue {
	return attribute.String(attrIdentity, identity)
}

func verbAttr(verb string) attribute.KeyValue {
	return attribute.String(attrVerb, verb)
}

func resourceAttr(resource string) attribute.KeyValue {
	return attribute.String(attrResource, resource)
}

func createdAttr(created bool) attribute.KeyValue {
	return attribute.Bool(attrCreated, created)
}

func outcomeAttr(outcome string) attribute.KeyValue {
	return attribute.String(attrOutcome, outcome)
}

func resultAttr(result string) attribute.KeyValue {
	return attribute.String(attrResult, result)
}

func directionAttr(direction string) attribute.KeyValue {
	return attribute.String(attrDirection, direction)
}

func deploymentAttr(id string) attribute.KeyValue {
	return attribute.String(attrDeployment, id)
}

func poolAttr(id string) attribute.KeyValue {
	return attribute.String(attrPool, id)
}

func policyAttr(policy string) attribute.KeyValue {
	return attribute.String(attrPolicy, policy)
}

// normalizePath replaces dynamic path segments with placeholders so metric
// label cardinality stays bounded by the route table, not by resource IDs:
// /v1/jobs/abc → /v1/jobs/{id}; /v1/deployment-pools/py/activations/run-42 →
// /v1/deployment-pools/{id}/activations/{actId}.
func normalizePath(path string) string {
	for _, prefix := range []string{"/v1/jobs/", "/v1/deployments/", "/v1/deployment-pools/"} {
		rest, ok := strings.CutPrefix(path, prefix)
		if !ok || rest == "" {
			continue
		}
		normalized := prefix + "{id}"
		// Keep the fixed sub-resource segment (traffic, revisions,
		// activations, ...), normalize the item after it.
		if _, after, found := strings.Cut(rest, "/"); found && after != "" {
			sub, item, _ := strings.Cut(after, "/")
			normalized += "/" + sub
			if item != "" {
				normalized += "/{actId}"
			}
		}
		return normalized
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
