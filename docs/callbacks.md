# Callbacks Guide

The orchestrator reports results by HTTP POST to a webhook you provide — on jobs, deployments, and pool activations alike. Every event is a [CloudEvents 1.0](https://cloudevents.io/) envelope, optionally signed with HMAC-SHA256, delivered asynchronously with retries and a circuit breaker.

## Configuring a callback

The `callback` object is the same everywhere it appears (job spec, deployment spec, activation request):

```json
{
  "callback": {
    "url": "https://your-service.example.com/webhook",
    "key": "your-hmac-secret",
    "events": ["orchestrator.job.exit"]
  }
}
```

- `url` — your webhook endpoint.
- `key` — optional HMAC-SHA256 signing key; see [verifying signatures](#verifying-signatures).
- `events` — optional filter; empty means all events for that resource.

Delivery is at-least-once per attempt policy but **at-most-once end to end**: failed deliveries are retried with backoff, but the orchestrator stores nothing durably — an orchestrator crash mid-delivery can drop an event. Design your consumer to reconcile via the query APIs rather than assuming a perfect stream.

## The envelope

```
POST /webhook HTTP/1.1
Content-Type: application/cloudevents+json
Ce-Specversion: 1.0
Ce-Type: orchestrator.job.exit
Ce-Subject: my-job-123
X-Signature-256: sha256=ab12...
```

```json
{
  "specversion": "1.0",
  "type": "orchestrator.job.exit",
  "source": "orchestrator/service",
  "subject": "my-job-123",
  "id": "my-job-123-1234567890",
  "time": "2026-01-15T10:30:00.000Z",
  "datacontenttype": "application/json",
  "data": { }
}
```

`type` tells you what happened, `subject` which resource, and `data` the payload — schemas below. `source` is `orchestrator/service` for job events and `orchestrator/deployments` for deployment and pool events.

## Event types

### Jobs

| Type | When | `data` |
| --- | --- | --- |
| `orchestrator.job.start` | Worker container started | `{"jobId", "meta"}` |
| `orchestrator.job.log` | Batch of stdout/stderr lines | `{"jobId", "lines": [...], "stream": "stdout\|stderr", "meta"}` |
| `orchestrator.job.artifact` | One artifact finished | `{"jobId", "artifactId", "artifactType", "status": "success\|failed", "content", "error", "meta"}` |
| `orchestrator.job.exit` | Worker exited | `{"jobId", "exitCode", "image", "durationSeconds", "error", "meta"}` |

`content` on artifact events carries the payload of `read` and `list` artifacts. `exitCode` is `-1` when the job failed before the worker could run (image pull failure, sidecar crash), with the reason in `error`. `meta` echoes the job's `meta` map for correlation.

### Deployments

| Type | When | `data` |
| --- | --- | --- |
| `orchestrator.deployment.response` | An [async request](deployments.md#async-requests) completed | `{"deploymentId", "invocationId", "statusCode", "body", "bodyEncoding", "bodyTruncated", "error"}` |

`invocationId` matches the `X-Invocation-Id` header from the original `202`. `body` is the workload's response body; when it isn't valid UTF-8 it arrives base64-encoded with `"bodyEncoding": "base64"`, and bodies over 1 MiB are truncated with `"bodyTruncated": true`. If the request never reached a replica (cold-start timeout, forward failure), `statusCode` and `body` are absent and `error` says why.

### Pools

| Type | When | `data` |
| --- | --- | --- |
| `orchestrator.pool.activation.result` | An [async activation](pools.md#async-activations) finished | `{"poolId", "activationId", "status", "exitCode", "output", "url", "error"}` |

Exec pools report `exitCode` and `output`; HTTP pools report the serving `url`. `status` is the activation's final [lifecycle state](pools.md#activation-lifecycle).

## Verifying signatures

When `callback.key` is set, every delivery carries `X-Signature-256: sha256=<hex>` — the HMAC-SHA256 of the exact request body under your key. Always compare in constant time:

```go
func verify(body []byte, signature, key string) bool {
    mac := hmac.New(sha256.New, []byte(key))
    mac.Write(body)
    expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(signature), []byte(expected))
}
```

```php
function verify(string $body, string $signature, string $key): bool {
    $expected = 'sha256=' . hash_hmac('sha256', $body, $key);
    return hash_equals($expected, $signature);
}
```

```python
def verify(body: bytes, signature: str, key: str) -> bool:
    expected = "sha256=" + hmac.new(key.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(signature, expected)
```

Signing keys are stored encrypted at rest on the Kubernetes backend (in Secrets, never in ConfigMaps or pod annotations) and are never echoed back through the query APIs.

## Consumer checklist

- Respond `2xx` quickly; do the work after acknowledging. Non-2xx responses are retried with backoff until the circuit breaker opens.
- Verify `X-Signature-256` before trusting the payload.
- Deduplicate on the CloudEvent `id` if your handler isn't idempotent.
- Filter with `events` at configuration time rather than discarding traffic at your endpoint — `orchestrator.job.log` in particular can be chatty.
