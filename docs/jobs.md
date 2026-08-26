# Jobs Guide

A **job** runs a container to completion: the orchestrator pulls your image, materializes input artifacts into a shared workspace, runs your command, processes output artifacts, and reports everything to your [callback](callbacks.md). Jobs survive orchestrator restarts — in-flight work is resumed, not lost.

Base URL: `http://localhost:8080` (default). When an API key is configured, send `Authorization: Bearer <key>`. Request bodies with unknown fields are rejected with `400` naming the field — a typo never silently runs with defaults.

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

The worker's exit decides the terminal status — code 0 is `completed`, anything else `failed` — and it decides it as soon as the worker exits, while post-job artifacts may still be uploading. Both backends answer the same way, and so does the callback for that exit. Status never moves backwards.

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
GET /readyz  # Readiness probe (checks backend connectivity — Docker daemon or K8s API server)
```

## Artifacts

Artifacts handle file operations before and after job execution. An artifact runs **before** the job by default, or **after** the job if it depends on `"job"` (directly or transitively).

### Artifact Types

| Type | Description |
|------|-------------|
| `download` | Download file from URL |
| `clone` | Materialize the tree at a git ref |
| `write` | Write inline content |
| `unarchive` | Extract a tar (plain/gzip/zstd/lz4), squashfs, or erofs archive |
| `mount` | Mount a squashfs or erofs image read-only into the workspace |
| `upload` | Upload file to URL |
| `read` | Include file contents in callback event |
| `archive` | Create tar, squashfs, or erofs archive |
| `list` | List files with glob pattern exclusions |
| `stat` | Include a file's size (bytes) in callback event |

### Common Fields

All artifacts use standardized `in` and `out` fields:

| Field | Description |
|-------|-------------|
| `id` | Unique artifact identifier (required) |
| `in` | Input - source URL, path, or content depending on type |
| `out` | Output - destination URL or path depending on type |
| `depends` | ID of artifact to wait for, or `"job"` for post-job execution |

### Clone Artifact

Materialize the tree at a git ref — for repositories whose provider hands out no archive URLs, where a `download` + `unarchive` chain has nothing to fetch. The clone is shallow, single-ref, and tagless, and the `.git` directory is removed after checkout: the workspace receives the tree, not a repository.

```json
{
  "id": "source",
  "type": "clone",
  "in": "https://git.example.com/acme/app.git",
  "out": "src",
  "ref": "main",
  "subdir": "packages/web",
  "headers": {"Authorization": "Bearer <token>"}
}
```

- `in` - Repository URL, http or https (required). Must not carry credentials — pass an `Authorization` header instead, so the token never rides a string that errors and logs echo verbatim.
- `out` - Directory to materialize the tree into (required). The tree is merged into whatever the directory already holds — directory meets directory merges, a same-named file is replaced, and everything else is left alone — the way `unarchive` extracts into an existing directory. The clone itself runs in a scratch directory, so a failed clone leaves the destination untouched.
- `ref` - Branch name, tag name, or full 40-hex commit hash (optional; defaults to the remote's default branch). A name is resolved as a branch first and a tag second. A commit hash reaches any commit the server is willing to serve — git forges generally allow reachable commits, not just branch and tag tips.
- `subdir` - Keep only this subdirectory of the tree (optional). A subdir that matches nothing fails rather than succeeding with the wrong tree, the same contract `unarchive`'s `subdir` keeps.
- `headers` - Headers sent with every git request (optional), typically `Authorization`
- `timeoutSeconds` - Timeout for the whole clone (default 300)

### Download Artifact

Download a file from a URL:

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

Write inline content to a file:

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

Extract an archive — tar (plain, gzip-, zstd-, or lz4-compressed), squashfs, or erofs — detected automatically from the archive's magic bytes. This materializes the files into the workspace. (To mount a squashfs or erofs image read-only *in place* instead of copying its files out, use the Mount artifact.)

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
- `subdir` - Extract only this subdirectory from the archive (optional; `./` prefixes and trailing slashes are normalized)
- `strip` - Drop the first path component of every entry (optional)

Git-forge archive downloads wrap the tree in a single root directory whose name varies by provider (GitHub uses `repo-main/`, Gitea uses `repo/`). Set `strip` to unwrap it without knowing its name:

```json
{
  "id": "extract-code",
  "type": "unarchive",
  "in": "repo.tar.gz",
  "out": "code",
  "strip": true,
  "depends": "download-code"
}
```

With `strip`, `subdir` is resolved against the unwrapped tree. If `strip` or `subdir` filtering leaves nothing to extract — `strip` on a flat archive with no wrapper directory, or a `subdir` that matches no entries — the artifact fails rather than succeeding with an empty destination. Combining both is useful for extracting specific folders from a forge archive:

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
      "strip": true,
      "depends": "download-template"
    }
  ]
}
```

This downloads the templates repo archive and extracts only the `nextjs/` subdirectory into `code/`, dropping the archive's root folder (`templates-main/`). For backward compatibility, a tar's root folder is also implicitly prepended to `subdir` when `strip` is not set — but new callers should be explicit.

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

### Mount Artifact

Mount a squashfs or erofs image read-only into the workspace, so the worker reads it directly without extraction (preserving the read-only image); the format is detected automatically from the image's magic bytes:

```json
{
  "id": "dataset",
  "type": "mount",
  "in": "dataset.sqfs",
  "out": "mnt/dataset"
}
```

This mounts `dataset.sqfs` at `mnt/dataset/` in the workspace, visible to the worker for its whole run and unmounted afterwards.

Set `writable: true` to give the worker a writable copy-on-write view: the read-only squashfs becomes the lower layer of an overlay whose upper layer lives on a tmpfs (the classic squashfs + tmpfs live-system pattern). The image is never modified; writes land in RAM (counted against the pod's memory limit) and are discarded when the job ends. Use `size` to cap that tmpfs in MiB — an overrun then fails with a disk-full error instead of OOM-killing the pod.

```json
{
  "id": "dataset",
  "type": "mount",
  "in": "dataset.sqfs",
  "out": "mnt/dataset",
  "writable": true,
  "size": 512
}
```

**Options:**
- `in` - Squashfs or erofs image to mount (required)
- `out` - Mount point directory in the workspace (required)
- `writable` - Overlay a writable layer on the image (optional, default read-only)
- `size` - Cap the writable overlay in MiB (optional, writable only; 0/omitted = kernel default of half of RAM)
- `sync` - URL to keep the writable layer at across runs (optional, writable only). The delta is restored before the worker starts and pushed while it runs; see [keeping a workspace](sandboxes.md#keeping-a-workspace-across-sandboxes) for what that does and does not promise.
- `syncIntervalSeconds` - How often to push the delta (optional, `sync` only; default 60, minimum 5)

> **Every workload kind.** Mounts work for jobs, [deployment revisions](deployments.md#the-request-spec), [pool activations](pools.md#artifacts) and [sandboxes](sandboxes.md#mounting-a-filesystem-image) alike — the same sidecar establishes them in all four. A job or a revision infers the capability from its artifacts, because its pod is built for the request; a pool has to declare `mounts: true`, because its pods stand warm long before the claim arrives.

> **Operator note:** Mounting activates automatically for any job whose artifacts include a `mount` entry — no configuration required. Such jobs require the matching kernel module on nodes (`squashfs` or `erofs`, plus `overlay` for writable mounts), and their sidecar runs privileged with mount propagation. Privilege is added only to the sidecar of jobs that mount — never to the worker, and never to other jobs. Loop devices are a finite per-node resource, so a node running many mounts at once may need `max_loop` raised; a job that cannot get one fails with `no free loop device`.

### Upload Artifact

Upload a file to a presigned URL:

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

Include file contents in the callback event:

```json
{
  "id": "metrics",
  "type": "read",
  "in": "metrics.json",
  "format": "json",
  "depends": "job"
}
```

- `in` - Path to read from (required)
- `format` - `text` (default) or `json` (optional)

The file contents are included in the `orchestrator.job.artifact` event's `content` field — always a raw string by default. With `format: "json"` the contents are delivered as the decoded JSON value, and the artifact fails if the file is not valid JSON.

### Archive Artifact

Create a tar, squashfs, or erofs archive from a file or directory:

```json
{
  "id": "archive",
  "type": "archive",
  "in": "output",
  "out": "output.tar.gz",
  "format": "tar",
  "compression": "gzip",
  "level": 5,
  "depends": "job"
}
```

- `in` - Source file or directory (required)
- `out` - Destination archive path (required)
- `format` - Container format, one of `"tar"`, `"squashfs"`, or `"erofs"` (required)
- `compression` - Compression algorithm. `tar`: `gzip`, `zstd`, `lz4`, or `lz4hc` (defaults to none); `squashfs`: `gzip`, `zstd`, `lz4`, or `lz4hc` (always compressed, defaults to `gzip`); `erofs`: `lz4` or `lz4hc` (defaults to none; compressed erofs also packs small files into a shared fragment area and dedupes identical data). `lz4hc` is the LZ4 high-compression encoder: better ratio for more compress-time CPU, identical decompression (optional)
- `level` - gzip compression level, `1`-`9`. Only valid when `compression` is `gzip` (optional)
- `blockSize` - squashfs block size in bytes, a power of 2 from `4096` (4 KiB) to `1048576` (1 MiB). Only valid for `squashfs`; defaults to `1048576` (optional)

Create a squashfs archive with zstd compression:

```json
{
  "id": "archive",
  "type": "archive",
  "in": "output",
  "out": "output.sqfs",
  "format": "squashfs",
  "compression": "zstd",
  "depends": "job"
}
```

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
      "format": "tar",
      "compression": "gzip",
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

Post-job artifacts wait briefly (a few seconds) for their source file to appear, then fail — the worker has already exited, so a file that isn't there yet will never be. Make sure the worker writes its outputs before exiting.

## Callbacks

Add a `callback` to receive lifecycle events — container started, log batches, per-artifact results, and the final exit — as signed CloudEvents:

```json
{
  "callback": {
    "url": "https://your-service.example.com/webhook",
    "key": "your-hmac-secret",
    "events": ["orchestrator.job.exit", "orchestrator.job.artifact"]
  }
}
```

Event schemas, the envelope format, and signature verification live in the [callbacks guide](callbacks.md).

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
| 400 | Invalid request — malformed JSON, unknown field, or failed validation; the message names the offending field |
| 401 | Missing or invalid API key |
| 404 | Job not found |
| 409 | Job with this ID already exists |
| 415 | `Content-Type` is not `application/json` |
| 500 | Internal error |
