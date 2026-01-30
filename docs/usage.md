# Orchestrator Usage Guide

This guide covers how to interact with the orchestrator service as an API client.

## API Overview

Base URL: `http://localhost:8080` (default)

All endpoints require the `Authorization: Bearer <token>` header when `API_KEY_FILE` is configured.

## Endpoints

### Create Job

```
POST /v1/jobs
```

**Request Body:**

```json
{
  "id": "my-job-123",
  "meta": {
    "userId": "user-456",
    "requestId": "req-789"
  },
  "image": "alpine:latest",
  "command": "sh -c 'echo hello > /workspace/output.txt'",
  "cpu": 0.5,
  "memory": 512,
  "environment": {
    "MY_VAR": "value"
  },
  "timeoutSeconds": 300,
  "workspace": "/workspace",
  "artifacts": [...],
  "callback": {...}
}
```

**Response:** `202 Accepted`

```json
{
  "id": "my-job-123",
  "status": "accepted"
}
```

### Get Job Status

```
GET /v1/jobs/{jobId}
```

**Response:**

```json
{
  "id": "my-job-123",
  "status": "completed",
  "exitCode": 0
}
```

Status values: `accepted`, `running`, `completed`, `failed`, `cancelled`

### List Jobs

```
GET /v1/jobs
```

**Response:**

```json
{
  "jobs": [
    {"id": "job-1", "status": "running"},
    {"id": "job-2", "status": "completed", "exitCode": 0}
  ]
}
```

### Cancel Job

```
DELETE /v1/jobs/{jobId}
```

**Response:** `204 No Content`

### Health Checks

```
GET /livez   # Liveness probe
GET /readyz  # Readiness probe (checks Docker connectivity)
```

## Artifacts

Artifacts handle file operations before and after job execution. An artifact runs **before** the job by default, or **after** the job if it depends on `"job"` (directly or transitively).

### Artifact Types

| Type | Description | When |
|------|-------------|------|
| `download` | Download file from URL | Pre-job |
| `write` | Write inline content | Pre-job |
| `unarchive` | Extract tar.gz archive | Pre-job |
| `upload` | Upload file to URL | Post-job (requires `depends: "job"`) |
| `read` | Include file in callback | Post-job (requires `depends: "job"`) |
| `archive` | Create tar.gz archive | Post-job (requires `depends: "job"`) |
| `list` | List files with glob excludes | Either (typically post-job) |

### Common Fields

All artifacts use standardized `in` and `out` fields:

| Field | Description |
|-------|-------------|
| `id` | Unique artifact identifier (required) |
| `in` | Input - source URL, path, or content depending on type |
| `out` | Output - destination URL or path depending on type |
| `depends` | ID of artifact to wait for, or `"job"` for post-job execution |

### Download Artifact

Download a file from a URL before the job starts:

```json
{
  "id": "model-weights",
  "type": "download",
  "in": "https://example.com/weights.bin",
  "out": "models/weights.bin"
}
```

- `in` - URL to download from (required)
- `out` - Path to write to (required)

### Write Artifact

Write content directly before the job starts:

```json
{
  "id": "config",
  "type": "write",
  "in": "{\"key\": \"value\"}",
  "out": "config.json"
}
```

- `in` - Content to write (required)
- `out` - Path to write to (required)

### Unarchive Artifact

Extract a tar.gz archive before the job starts:

```json
{
  "id": "code",
  "type": "unarchive",
  "in": "code.tar.gz",
  "out": "src"
}
```

This extracts `code.tar.gz` into the `src/` directory.

**Options:**
- `in` - Archive file to extract (required)
- `out` - Destination directory (required)
- `subdir` - Extract only this subdirectory from the archive (optional)

The `subdir` option is useful for extracting specific folders from GitHub archive downloads, which have a root folder like `repo-main/`:

```json
{
  "artifacts": [
    {
      "id": "download-template",
      "type": "download",
      "in": "https://github.com/org/templates/archive/main.tar.gz",
      "out": "templates.tar.gz"
    },
    {
      "id": "extract-nextjs",
      "type": "unarchive",
      "in": "templates.tar.gz",
      "out": "code",
      "subdir": "nextjs",
      "depends": "download-template"
    }
  ]
}
```

This downloads the templates repo archive and extracts only the `nextjs/` subdirectory into `code/`. The archive root folder (`templates-main/`) is automatically detected and stripped.

Often chained with a download:

```json
{
  "artifacts": [
    {
      "id": "download-code",
      "type": "download",
      "in": "https://example.com/code.tar.gz",
      "out": "code.tar.gz"
    },
    {
      "id": "extract-code",
      "type": "unarchive",
      "in": "code.tar.gz",
      "out": "src",
      "depends": "download-code"
    }
  ]
}
```

### Upload Artifact

Upload a file to a presigned URL after the job completes:

```json
{
  "id": "result",
  "type": "upload",
  "in": "output.tar.gz",
  "out": "https://storage.example.com/presigned-upload-url",
  "depends": "job"
}
```

- `in` - Path to read from (required)
- `out` - URL to upload to (required)

### Read Artifact

Include file contents in the callback event after the job completes:

```json
{
  "id": "metrics",
  "type": "read",
  "in": "metrics.json",
  "depends": "job"
}
```

- `in` - Path to read from (required)

The file contents (parsed as JSON if valid, otherwise string) are included in the `orchestrator.job.artifact` event's `content` field.

### Archive Artifact

Create a tar.gz archive from a file or directory after the job completes:

```json
{
  "id": "archive",
  "type": "archive",
  "in": "output",
  "out": "output.tar.gz",
  "format": "tar.gz",
  "depends": "job"
}
```

- `in` - Source file or directory (required)
- `out` - Destination archive path (required)
- `format` - Archive format, must be `"tar.gz"` (required)

### List Artifact

List files in a directory, optionally recursively, with glob pattern exclusions. Returns the list of file paths in the callback event.

```json
{
  "id": "file-manifest",
  "type": "list",
  "in": "output",
  "recursive": true,
  "excludes": ["node_modules", ".git", "*.log"],
  "depends": "job"
}
```

**Options:**
- `in` - Directory to list (required)
- `recursive` - Recurse into subdirectories (default: `true`)
- `excludes` - Glob patterns to exclude (matches file/directory names)

The artifact event content will be an array of relative file paths:

```json
{
  "type": "orchestrator.job.artifact",
  "data": {
    "artifactId": "file-manifest",
    "artifactType": "list",
    "status": "success",
    "content": [
      "src/main.go",
      "src/utils/helper.go",
      "package.json"
    ]
  }
}
```

### Artifact Dependencies

Use `depends` to chain artifacts. The dependent artifact waits for its dependency to complete.

**Pre-job chaining** (download then extract):

```json
{
  "artifacts": [
    {
      "id": "download",
      "type": "download",
      "in": "https://example.com/code.tar.gz",
      "out": "code.tar.gz"
    },
    {
      "id": "extract",
      "type": "unarchive",
      "in": "code.tar.gz",
      "out": "code",
      "depends": "download"
    }
  ]
}
```

**Post-job chaining** (archive then upload):

```json
{
  "artifacts": [
    {
      "id": "archive",
      "type": "archive",
      "in": "build",
      "out": "build.tar.gz",
      "format": "tar.gz",
      "depends": "job"
    },
    {
      "id": "upload",
      "type": "upload",
      "in": "build.tar.gz",
      "out": "https://storage.example.com/upload",
      "depends": "archive"
    }
  ]
}
```

## Callbacks

Receive lifecycle events via HTTP POST to your endpoint.

```json
{
  "callback": {
    "url": "https://your-service.example.com/webhook",
    "key": "your-hmac-secret",
    "events": ["orchestrator.job.start", "orchestrator.job.exit"]
  }
}
```

- `url` - Your webhook endpoint
- `key` - (Optional) HMAC-SHA256 signing key for verifying events
- `events` - (Optional) Filter which events to receive. Empty = all events.

### Event Types

| Type | Description |
|------|-------------|
| `orchestrator.job.start` | Job container started |
| `orchestrator.job.artifact` | Artifact processed (success/failed) |
| `orchestrator.job.log` | Log lines from stdout/stderr |
| `orchestrator.job.exit` | Job container exited |

### CloudEvent Format

Events follow the [CloudEvents 1.0 specification](https://cloudevents.io/).

**Headers:**

```
Content-Type: application/cloudevents+json
Ce-Specversion: 1.0
Ce-Type: orchestrator.job.exit
Ce-Source: orchestrator/supervisor
Ce-Subject: my-job-123
Ce-Id: my-job-123-1234567890
Ce-Time: 2024-01-15T10:30:00Z
X-Signature-256: sha256=abc123...
```

**Body:**

```json
{
  "specversion": "1.0",
  "type": "orchestrator.job.exit",
  "source": "orchestrator/supervisor",
  "subject": "my-job-123",
  "id": "my-job-123-1234567890",
  "time": "2024-01-15T10:30:00.000Z",
  "datacontenttype": "application/json",
  "data": {
    "jobId": "my-job-123",
    "exitCode": 0,
    "meta": {
      "userId": "user-456",
      "requestId": "req-789"
    }
  }
}
```

### Event Data Schemas

**start:**
```json
{"jobId": "...", "meta": {...}}
```

**artifact:**
```json
{"jobId": "...", "artifactId": "...", "artifactType": "download|upload|write|read|archive|unarchive|list", "status": "success|failed", "content": ..., "error": "...", "meta": {...}}
```

**log:**
```json
{"jobId": "...", "lines": ["line1", "line2"], "stream": "stdout|stderr", "meta": {...}}
```

**exit:**
```json
{"jobId": "...", "exitCode": 0, "error": "...", "meta": {...}}
```

### Verifying Signatures

When `callback.key` is set, events are signed with HMAC-SHA256. Verify the `X-Signature-256` header:

```go
func verifySignature(body []byte, signature, key string) bool {
    mac := hmac.New(sha256.New, []byte(key))
    mac.Write(body)
    expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(signature), []byte(expected))
}
```

```php
function verifySignature(string $body, string $signature, string $key): bool {
    $expected = 'sha256=' . hash_hmac('sha256', $body, $key);
    return hash_equals($expected, $signature);
}
```

## Complete Example

```json
{
  "id": "video-transcode-001",
  "meta": {
    "userId": "user-123",
    "projectId": "proj-456"
  },
  "image": "ffmpeg:latest",
  "command": "ffmpeg -i /workspace/input.mp4 -c:v libx264 /workspace/output.mp4",
  "cpu": 4,
  "memory": 4096,
  "timeoutSeconds": 3600,
  "artifacts": [
    {
      "id": "source-video",
      "type": "download",
      "in": "https://storage.example.com/videos/source.mp4",
      "out": "input.mp4"
    },
    {
      "id": "transcoded-video",
      "type": "upload",
      "in": "output.mp4",
      "out": "https://storage.example.com/upload/output.mp4?signature=...",
      "depends": "job"
    }
  ],
  "callback": {
    "url": "https://api.example.com/webhooks/jobs",
    "key": "whsec_abc123",
    "events": ["orchestrator.job.exit", "orchestrator.job.artifact"]
  }
}
```

## Error Responses

All errors return JSON:

```json
{
  "error": "Job not found"
}
```

| Status | Meaning |
|--------|---------|
| 400 | Invalid request |
| 401 | Missing/invalid API key |
| 404 | Job not found |
| 409 | Job already exists |
| 500 | Internal error |
