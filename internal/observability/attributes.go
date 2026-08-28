// Package observability provides metrics, tracing, and logging utilities.
package observability

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Attribute keys
const (
	attrMethod      = "method"
	attrPath        = "path"
	attrStatus      = "status"
	attrImage       = "image"
	attrSuccess     = "success"
	attrIdentity    = "identity"
	attrVerb        = "verb"
	attrResource    = "resource"
	attrCreated     = "created"
	attrComponent   = "component" // which component held the request
	attrOutcome     = "outcome"
	attrResult      = "result"
	attrDirection   = "direction"
	attrDeployment  = "deployment"
	attrPool        = "pool"
	attrKind        = "kind" // which warm-pool consumer: pool | sandbox
	attrPolicy      = "policy"
	attrType        = "type"
	attrFormat      = "format"
	attrCompression = "compression"
)

func methodAttr(method string) attribute.KeyValue {
	return attribute.String(attrMethod, method)
}

func pathAttr(path string) attribute.KeyValue {
	return attribute.String(attrPath, path)
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

func componentAttr(component string) attribute.KeyValue {
	return attribute.String(attrComponent, component)
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

func kindAttr(kind string) attribute.KeyValue {
	return attribute.String(attrKind, kind)
}

func poolAttr(id string) attribute.KeyValue {
	return attribute.String(attrPool, id)
}

func policyAttr(policy string) attribute.KeyValue {
	return attribute.String(attrPolicy, policy)
}

func typeAttr(value string) attribute.KeyValue {
	return attribute.String(attrType, value)
}

func formatAttr(value string) attribute.KeyValue {
	return attribute.String(attrFormat, value)
}

func compressionAttr(value string) attribute.KeyValue {
	return attribute.String(attrCompression, value)
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
