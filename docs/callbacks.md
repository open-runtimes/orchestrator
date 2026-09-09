# Callbacks Guide

The orchestrator reports results by HTTP POST to a webhook you provide — on jobs and deployments alike. Every event is a [CloudEvents 1.0](https://cloudevents.io/) envelope, optionally signed with HMAC-SHA256, delivered asynchronously with retries and a circuit breaker.

## Configuring a callback

The `callback` object is the same everywhere it appears (job and deployment specs):

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

`type` tells you what happened, `subject` which resource, and `data` the payload — schemas below. `source` is `orchestrator/service` for job events and `orchestrator/deployments` for deployment events.

## Event types

### Jobs

| Type | When | `data` |
| --- | --- | --- |
| `orchestrator.job.start` | Worker container started | `{"jobId", "meta"}` |
| `orchestrator.job.log` | Batch of stdout/stderr lines | `{"jobId", "lines": [...], "stream": "stdout\|stderr", "meta"}` |
| `orchestrator.job.artifact` | One artifact finished | `{"jobId", "artifactId", "artifactType", "status": "success\|failed", "content", "durationSeconds", "error", "meta"}` |
| `orchestrator.job.exit` | Worker exited | `{"jobId", "exitCode", "reason", "image", "durationSeconds", "error", "meta"}` |
| `orchestrator.job.complete` | Post-job artifacts finished | `{"jobId", "meta"}` |

`content` on artifact events carries the payload of `read` and `list` artifacts: a raw string for `read` (or the decoded JSON value when the artifact sets `format: "json"`), an array of paths for `list`. `exitCode` is `-1` when the job failed before the worker could run (image pull failure, sidecar crash), with `error.code: "job_failed"` and backend detail in `reason`. `reason` names why the worker terminated when the backend can attest to a cause beyond the exit code — currently only `"oom"` (killed by the kernel OOM killer) — and is omitted otherwise, so an exit code alone (e.g. an ambiguous 137) is never over-interpreted. Treat unknown `reason` values as if the field were absent; new causes may be added. `meta` echoes the job's `meta` map for correlation. `exit` fires as soon as the worker's command exits; `complete` fires after every post-job artifact has been processed — including ones that failed. It means "no more events for this job", not that every artifact succeeded: wait for it before fetching artifacts, but join on each `orchestrator.job.artifact` event's `status` to know whether an artifact actually landed. Jobs that fail before the worker runs emit `exit` (with `exitCode: -1`) but no `complete`.

### The `error` object

Every callback that reports a failure carries the same `error` object, and omits
it entirely on success:

```json
{
  "error": {
    "code": "archive_unknown_format",
    "message": "Unrecognized archive format for source.tar.gz"
  }
}
```

`code` is a lower snake-case enum — branch on it. `message` is a sentence for a
person: detail about this one occurrence (the offending path, an HTTP status, a
decoder's complaint) in terms of jobs and deployments, never of the backend
running them. Show it, never parse it, and expect its wording to change.

#### Job error codes

| Artifact `error.code` | Meaning |
| --- | --- |
| `archive_empty` | The archive has no bytes. |
| `archive_unknown_format` | The input format is not recognized, or an unsupported output format was requested. |
| `archive_corrupt` | A recognized tar/gzip decoder reports malformed or incomplete data. |
| `archive_path_invalid` | An archive entry or link violates extraction path rules. |
| `archive_layout_mismatch` | No entries match the requested root/strip layout. |
| `archive_extraction_failed` | Another extraction or decoder/tool failure; does not establish corruption. |
| `archive_creation_failed` | Archive creation failed. |
| `archive_compression_unsupported` | The requested output compression is unsupported. |
| `artifact_not_found` | A required filesystem path does not exist. |
| `artifact_permission_denied` | A filesystem operation was denied. |
| `artifact_timeout` | The artifact operation's context deadline expired. |
| `artifact_canceled` | The artifact operation's context was canceled. |
| `artifact_read_failed` | File reading failed for another reason. |
| `artifact_write_failed` | File writing failed for another reason. |
| `artifact_stat_failed` | File metadata lookup failed for another reason. |
| `artifact_list_failed` | Directory listing failed for another reason. |
| `artifact_json_invalid` | A JSON read artifact could not parse its input. |
| `download_http_error` | Download returned an unsuccessful HTTP status. |
| `download_failed` | Another download failure. |
| `upload_failed` | Upload failed. |
| `clone_failed` | Git clone/checkout failed. |
| `mount_failed` | Mount setup failed. |
| `artifact_failed` | Unclassified artifact failure. |

Exit callbacks use `job_exit_nonzero` for a nonzero process exit,
`job_oom` when the backend explicitly reports an OOM kill, and `job_failed` when
the backend reports failure without a process exit result (`exitCode: -1`).
The numeric `exitCode` and existing `reason` field retain their meanings.

**Compatibility:** `error` changed from a string to this object. Deploy consumers
that accept both before upgrading the service. During a rolling upgrade, an old
sidecar's diagnostic prose and unknown future codes are normalized by the service
to an operation-level `code`, with the original text kept as the `message`; the
text is never parsed for a more specific cause. New sidecar codes pass through
older services unchanged. Treat an unrecognized `code` as a generic failure and
fall back to `message`.

Subscribe to `orchestrator.job.artifact` even for manual uploads: source extraction
can fail before any worker logs exist. The artifact callback provides the cause;
`orchestrator.job.exit` provides the terminal outcome. Handle either delivery order
and deduplicate by CloudEvent ID.

### Deployments

| Type | When | `data` |
| --- | --- | --- |
| `orchestrator.deployment.response` | An [async request](deployments.md#async-requests) completed | `{"deploymentId", "invocationId", "requestMethod", "requestPath", "requestHeaders", "durationSeconds", "statusCode", "body", "bodyEncoding", "bodyTruncated", "error"}` |

`invocationId` matches the `X-Invocation-Id` header from the original `202`. `requestMethod`, `requestPath`, and `requestHeaders` echo the original request so a consumer can reconstruct its record from the callback alone — request headers double as a caller-defined metadata channel that round-trips. `requestHeaders` is a `{name: [values]}` map (repeated values are preserved); the orchestrator's own `Prefer`/`X-Invocation-Id` and credential headers (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`) are never echoed, and if the headers exceed a size cap they're dropped in favor of `"requestHeadersTruncated": true`. `requestPath` is likewise bounded — an over-long path+query is cut with `"requestPathTruncated": true` — so a large request target can't make the callback undeliverable. `durationSeconds` is the workload round-trip time (it excludes any cold-start wait) and is absent when the request never reached a replica. `body` is the workload's response body; when it isn't valid UTF-8 it arrives base64-encoded with `"bodyEncoding": "base64"`, and bodies over 1 MiB are truncated with `"bodyTruncated": true`. If the request never reached a replica (cold-start timeout, forward failure), `statusCode` and `body` are absent and `error` says why — `deployment_no_capacity` when nothing was ready in time to serve it, `deployment_forward_failed` when the request could not be delivered, and `deployment_response_unreadable` when the deployment answered but its body could not be read (there, `statusCode` is present).

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
