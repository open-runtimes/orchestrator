# Ubiquitous Language

## Jobs

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Job** | A single containerized workload: one worker container plus one sidecar, running to completion | Task, run, execution |
| **Request** | The client-supplied specification for a job: image, command, resources, artifacts, and callback config | Job spec, payload |
| **Workspace** | The shared directory mounted into both the worker and sidecar containers | Volume, working directory |
| **Worker** | The user-supplied container that runs the job command | User container, app container |
| **Sidecar** | The orchestrator-managed container that handles artifact processing and job lifecycle reporting | Helper container, agent |

## Job states

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Accepted** | A job has been persisted and queued but the worker has not yet started | Pending, queued |
| **Running** | The worker container is executing | In-progress, active |
| **Completed** | The worker exited with code 0 | Succeeded, finished |
| **Failed** | The worker exited with a non-zero code, or the sidecar crashed before the worker started | Errored |
| **Cancelled** | The job was stopped before natural completion | Aborted, killed |

## Artifacts

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Artifact** | A data operation associated with a job, ordered by a dependency chain | File operation, step, task |
| **Input artifact** | An artifact with no dependency on `job`; runs before the worker starts | Pre-artifact, pre-step |
| **Output artifact** | An artifact that depends (directly or transitively) on `job`; runs after the worker exits | Post-artifact, post-step |
| **Dependency** | The `depends` field value that orders artifacts; the special value `"job"` means "after the worker exits" | Parent, prerequisite |
| **ArtifactReport** | The payload posted by the sidecar to the orchestrator reporting the result of one artifact operation | Artifact result, artifact event |

## Artifact types

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Download** | Fetches a file from a URL into the workspace | Fetch, pull |
| **Upload** | Sends a file from the workspace to a URL | Push, put |
| **Write** | Writes inline content from the request into a workspace file | Inject, create |
| **Read** | Reads a workspace file and includes its content in the callback | Get, cat |
| **Archive** | Compresses a workspace directory into a tar.gz file | Zip, pack |
| **Unarchive** | Expands a tar.gz file into the workspace | Unzip, unpack, extract |
| **List** | Enumerates files in a workspace directory and includes the list in the callback | Ls, dir |

## Callbacks

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Callback** | An HTTP webhook the orchestrator calls to report job lifecycle events | Webhook, notification, event |
| **CloudEvent** | The CloudEvents 1.0 envelope used to structure each callback payload | Event, message |
| **Event type** | The string classifying a callback (`orchestrator.job.start`, `.artifact`, `.log`, `.exit`) | Event name, topic |
| **Signing key** | An HMAC-SHA256 secret used to sign callback payloads for verification | Secret, token, API key |
| **Event filter** | The `events` list on a Callback that restricts which event types are delivered; empty means all | Subscription, event mask |

## Signals

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Signal** | A sealed backend-agnostic type emitted by a LifecycleWatcher to describe one moment in a job's execution | Event (conflicts with CloudEvent) |
| **Started** signal | Emitted when the worker container has started successfully | Launch signal |
| **Exited** signal | Emitted when the worker container exits, carrying exit code and duration | Finished signal, done signal |
| **Failed** signal | Emitted when the job fails before or without the worker starting (sidecar crash, image pull failure) | Error signal |
| **LogLine** signal | Emitted for each batch of stdout/stderr lines from the worker | Log signal, output signal |

## Infrastructure

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Orchestrator** | The component that provisions containers, mounts the workspace, and forwards signals to the controller | Runner, executor |
| **LifecycleWatcher** | Backend-specific component that translates native container events (Docker events, Kubernetes pod phases) into Signals | Monitor, observer |
| **Controller** | Processes Signals from the Orchestrator: updates job state and dispatches Callbacks | Handler, manager, supervisor |
| **Dispatcher** | Sends CloudEvent callbacks over HTTP with retries and circuit-breaking | Notifier, sender, emitter |
| **Store** | Persists job state and enforces FSM transition rules | Repository, database, state machine |
| **Registry** | Maps artifact type names (e.g. `"download"`) to constructors and validators | Factory, lookup table |

## Relationships

- A **Job** has exactly one **Worker** and one **Sidecar** container.
- A **Job** has zero or more **Artifacts**, ordered by the **Dependency** chain.
- **Input artifacts** run before the **Worker**; **output artifacts** run after.
- The **Sidecar** applies artifacts and posts **ArtifactReports** to the **Orchestrator**.
- The **LifecycleWatcher** emits **Signals**; the **Controller** consumes them to drive state transitions in the **Store** and fire **Callbacks** via the **Dispatcher**.
- A **Callback** carries a **Signing key** and an **Event filter**; the **Dispatcher** only delivers **CloudEvents** matching the filter.

## Example dialogue

> **Dev:** "When does a **job** move from **accepted** to **running**?"

> **Domain expert:** "When the **sidecar** finishes all **input artifacts** and the **worker** container starts. The **LifecycleWatcher** emits a **Started** signal, which the **Controller** uses to apply the state transition."

> **Dev:** "So the **Started** signal is what triggers the `orchestrator.job.start` **callback**?"

> **Domain expert:** "Exactly. The Controller calls `EmitCallback` with the **Started** signal, which builds a **CloudEvent** of type `orchestrator.job.start` and hands it to the **Dispatcher**. If the **event filter** on the **callback** config doesn't include that type, it's silently dropped."

> **Dev:** "What if the **sidecar** crashes before the **worker** starts?"

> **Domain expert:** "The **LifecycleWatcher** emits a **Failed** signal — not **Exited**. **Failed** means we never got a worker exit code. The **job** goes to the **failed** state, and the `orchestrator.job.exit` **CloudEvent** is sent with exit code -1."

> **Dev:** "And **output artifacts** — they run after that exit event?"

> **Domain expert:** "After the worker exits, yes. The **sidecar** processes **output artifacts** and posts an **ArtifactReport** for each one. Those trigger `orchestrator.job.artifact` **CloudEvents** before the final exit callback."

## Flagged ambiguities

- **"event"** is overloaded: it refers to both a **CloudEvent** (the HTTP callback payload) and a **Signal** (the internal Go type emitted by the LifecycleWatcher). In code, prefer **Signal** for the internal type and **CloudEvent** or **callback** for the external one.
- **"status"** appears as both a job **state** (the FSM value: `accepted`, `running`, etc.) and as the `Status` struct returned by the API. In conversation, prefer **state** for the FSM concept and **status response** for the API type.
- **"error"** in an **ArtifactReport** is a string, while `artifact.Result.Error` is a Go `error`. In domain discussions, say **artifact failure reason** to avoid confusion with Go error handling.
