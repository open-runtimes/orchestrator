# Ubiquitous Language

## Planes

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Jobs plane** | The subsystem that runs finite containerized workloads to completion with callbacks | Batch side, jobs service |
| **Serving plane** | The subsystem that runs long-lived HTTP workloads behind the gateway | Deployments side |
| **Backend** | The infrastructure a plane targets: Docker or Kubernetes | Provider, driver, platform |

## Jobs

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Job** | A single containerized workload: one worker container plus one job sidecar, running to completion | Task, run, execution |
| **Request** | The client-supplied specification for a job: image, command, resources, artifacts, and callback config | Job spec, payload |
| **Workspace** | The shared directory mounted into both the worker and job sidecar containers | Volume, working directory |
| **Worker** | The user-supplied container that runs the job command | User container, app container |
| **Job sidecar** | The orchestrator-managed container that handles artifact processing and job lifecycle reporting | Sidecar (unqualified), helper container, agent |

## Job states

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Accepted** | A job has been persisted and queued but the worker has not yet started | Pending, queued |
| **Running** | The worker container is executing | In-progress, active |
| **Completed** | The worker exited with code 0 | Succeeded, finished |
| **Failed** | The worker exited with a non-zero code, or the job sidecar crashed before the worker started | Errored |
| **Cancelled** | The job was stopped before natural completion | Aborted, killed |

## Artifacts

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Artifact** | A data operation associated with a job, ordered by a dependency chain | File operation, step, task |
| **Input artifact** | An artifact with no dependency on `job`; runs before the worker starts | Pre-artifact, pre-step |
| **Output artifact** | An artifact that depends (directly or transitively) on `job`; runs after the worker exits | Post-artifact, post-step |
| **Dependency** | The `depends` field value that orders artifacts; the special value `"job"` means "after the worker exits" | Parent, prerequisite |
| **ArtifactReport** | The payload posted by the job sidecar to the orchestrator reporting the result of one artifact operation | Artifact result, artifact event |

## Artifact types

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Download** | Fetches a file from a URL into the workspace | Fetch, pull |
| **Upload** | Sends a file from the workspace to a URL | Push, put |
| **Write** | Writes inline content from the request into a workspace file | Inject, create |
| **Read** | Reads a workspace file and includes its content in the callback | Get, cat |
| **Archive** | Compresses a workspace directory into a tar or squashfs file | Zip, pack |
| **Unarchive** | Expands a tar (plain/gzip/zstd/lz4) or squashfs archive into the workspace | Unzip, unpack, extract |
| **Mount** | Mounts a squashfs image into the workspace for the worker, read-only or as a writable tmpfs overlay | Attach, bind |
| **List** | Enumerates files in a workspace directory and includes the list in the callback | Ls, dir |

## Callbacks

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Callback** | An HTTP webhook the orchestrator calls to report job, deployment, or activation events | Webhook, notification, event |
| **CloudEvent** | The CloudEvents 1.0 envelope used to structure each callback payload | Event, message |
| **Event type** | The string classifying a callback (`orchestrator.job.start`, `.artifact`, `.log`, `.exit`, `orchestrator.pool.activation.result`) | Event name, topic |
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

## Deployments

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Deployment** | A long-lived HTTP workload with an identity, a host, and a history of revisions | App, service, function |
| **Revision** | An immutable snapshot of a Deployment's spec, named `{id}-{NNNNN}`, individually routable | Version, release |
| **Host** | A hostname routing external traffic to exactly one Deployment; a Deployment may own several, the first being primary | Domain, URL, alias |
| **Marker** | The per-Deployment record of lifecycle state: latest revision, last ready revision, traffic mode | State ConfigMap, metadata |
| **Spec Secret** | The at-rest store of a Deployment's full spec, including secret material like signing keys | Spec annotation, marker spec |
| **Deployment sidecar** | The reverse proxy in every Revision replica: readiness, drain, concurrency cap, stats | Sidecar (unqualified), proxy, queue-proxy |
| **Gateway** | The Gateway API edge that terminates Hosts and applies the traffic table | Ingress, load balancer |

## Traffic

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Traffic table** | The weighted split of a Deployment's traffic across named Revisions | Routing, weights |
| **Traffic target** | One row of the traffic table: a Revision name and its percent | Leg, split entry |
| **Auto mode** | Traffic management where 100% cuts to each new Revision once it is ready | Default mode |
| **Manual mode** | Traffic management where the operator owns the traffic table until released back to auto | Canary mode, pinned |
| **Auto-cut** | The transition of all traffic to the latest ready Revision while in auto mode | Promotion, rollout |

## Cold path

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Warm** | The state of a Revision with at least one ready replica serving directly | Active, hot |
| **Cold** | The state of a Revision scaled to zero replicas | Idle, off |
| **Endpoint flip** | The swap of a Revision's endpoints between its ready pods (warm) and Activator pods (cold) | Slice swap, SKS flip |
| **Activator** | The buffering edge that holds cold and async requests, raises cold Revisions, and forwards | Buffer, edge proxy |
| **Raise** | The Activator's scale-up of a cold Revision from zero so a held request can be served | Wake, cold scale-up |
| **Cold start** | The end-to-end latency of a request that arrives while its Revision is cold | Spin-up time |
| **Async request** | A request marked `Prefer: respond-async`, accepted immediately and answered via the Callback | Background request |

## Autoscaling

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Autoscaler** | The concurrency-driven loop that owns 1↔N scaling and scale-to-zero (never 0→N — that is a raise) | KPA, HPA |
| **Concurrency** | The number of requests in flight in a replica, measured as a concurrency-seconds integral | Load, QPS, RPS |
| **Concurrency target** | The per-replica concurrency the Autoscaler aims for when sizing a Revision | Threshold, limit |
| **Scale to zero** | The Autoscaler's N→0 transition after a window with no concurrency, making the Revision cold | Idling, hibernation |
| **Drain** | The deployment sidecar's refusal of new requests while in-flight ones finish during shutdown | Graceful shutdown |

## Pools

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Pool** | A config-declared set of warm pods kept ready for instant activation | Warm pool, pre-warm fleet |
| **Warm pod** | A pool member running the Shim, waiting to be claimed | Standby pod, spare |
| **Shim** | The PID-1 entrypoint in a warm pod that blocks on a FIFO and execs the activation payload | Launcher, init |
| **Claim** | The atomic, token-authenticated take of one warm pod; the sidecar's accepted POST *is* the claim | Reservation, checkout |
| **Claim token** | The per-pod credential (HMAC of the pod name under the install key) that authorizes a claim | Secret, password |
| **Inventory** | What a Pool backend supplies to the shared claim module: how to list, create, and address warm units | Backend, provider |
| **Activation** | The materialization of artifacts and exec of a payload inside a claimed warm pod | Cold start, launch |
| **Poison** | The marking of a claimed pod as unusable after a failed activation so it is never reissued | Taint, quarantine |
| **Replenish** | The pool's creation of new warm pods to restore its declared size after claims | Refill, top-up |
| **Burst policy** | What an empty pool does with a claim: reject (429) or fall back to a cold start | Overflow |

## Security & placement

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Workload namespace** | The hardened namespace (restricted PSA, default-deny, quota) where workload pods run | Jobs namespace, tenant ns |
| **Release namespace** | The namespace holding the control plane: services, Activator, gateway wiring | System namespace |
| **Sandbox** | A workload's isolation tier — runc, gvisor, or kata — mapped to a RuntimeClass | Runtime, isolation level |
| **Overcommit** | The divisor deriving a workload's CPU request from its declared limit | Oversubscription ratio |

## Infrastructure

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Orchestrator** | The backend-specific component that provisions containers, mounts the workspace, and forwards signals | Runner, executor |
| **LifecycleWatcher** | Backend-specific component that translates native container events into Signals | Monitor, observer |
| **Controller** | Processes Signals from the Orchestrator: updates job state and dispatches Callbacks | Handler, manager, supervisor |
| **Dispatcher** | Sends CloudEvent callbacks over HTTP with retries and circuit-breaking | Notifier, sender, emitter |
| **Store** | Persists workload state and enforces FSM transition rules | Repository, database, state machine |
| **Registry** | Maps artifact type names (e.g. `"download"`) to constructors and validators | Factory, lookup table |

## Relationships

- A **Job** has exactly one **Worker** and one **Job sidecar**; it has zero or more **Artifacts**, ordered by the **Dependency** chain — **input artifacts** before the Worker, **output artifacts** after.
- The **LifecycleWatcher** emits **Signals**; the **Controller** consumes them to drive state transitions in the **Store** and fire **Callbacks** via the **Dispatcher**.
- A **Deployment** owns one or more **Hosts** (each Host belongs to exactly one Deployment) and one or more **Revisions**; its **Marker** tracks lifecycle and its **Spec Secret** holds the spec.
- The **Traffic table** distributes a **Deployment**'s traffic across **traffic targets**, each naming exactly one **Revision**.
- Every **Revision** replica pairs the workload with one **Deployment sidecar**; the **endpoint flip** decides whether the Revision's endpoints are its own pods (**warm**) or the **Activator** (**cold**).
- The **Activator** owns 0→N (**raise**); the **Autoscaler** owns 1↔N and **scale to zero** — the two never overlap.
- A **Pool** maintains N **warm pods**; a **Claim** takes one, an **Activation** turns it into a running workload, and the Pool **replenishes**; a failed activation **poisons** the pod.
- Both Pool backends delegate claiming to one shared claim module; each supplies an **Inventory** (Kubernetes: pool pods + HMAC tokens; Docker: slots + label tokens).
- Workload pods live in the **Workload namespace**; the control plane and **Activator** live in the **Release namespace**.

## Example dialogue

> **Dev:** "When does a **job** move from **accepted** to **running**?"
> **Domain expert:** "When the **job sidecar** finishes all **input artifacts** and the **worker** starts. The **LifecycleWatcher** emits a **Started** signal, and the **Controller** applies the transition and fires the `orchestrator.job.start` **CloudEvent** — unless the **event filter** drops it."
> **Dev:** "Different question — a request just hit a **cold** **Revision**. Who scales it up, the **Autoscaler**?"
> **Domain expert:** "No — the **Activator**. The **endpoint flip** already pointed the Revision's endpoints at the Activator, which holds the request and **raises** the Revision from zero. The Autoscaler only takes over once it's running: 1↔N, and eventually **scale to zero** again."
> **Dev:** "Is claiming a **warm pod** from a **Pool** also a raise?"
> **Domain expert:** "Different concept. A **claim** takes one warm pod out of the Pool and the **activation** execs your payload in it via the **Shim** — no scaling involved; the Pool just **replenishes**. A failed activation **poisons** the pod. An activation exists precisely to avoid a **cold start**."
> **Dev:** "And if the Pool is empty when I claim?"
> **Domain expert:** "That's the **burst policy**: reject with 429, or fall back to a cold start. Either way the result comes back on the **Callback**, same envelope as job events."

## Flagged ambiguities

- **"Deployment"** collides three ways: the domain entity, the Kubernetes `apps/v1 Deployment` that backs each Revision, and the verb "to deploy". Reserve the bare word for the domain entity; say "apps/v1 Deployment" for the Kubernetes object and "roll out" for the act.
- **"Sidecar"** unqualified is ambiguous across planes — the **Job sidecar** (artifacts/lifecycle) and the **Deployment sidecar** (reverse proxy) are unrelated programs. Qualify it whenever both planes are in scope; the binary names (`job-sidecar`, `deployments-sidecar`) already do.
- **"Activation" vs "cold start"** were used interchangeably during design. They are distinct: **Activation** is the pool mechanism (claim + exec in a warm pod); **cold start** is the latency of serving a request to a cold Revision via a **raise**.
- **"Idle" vs "cold"**: both described a scaled-to-zero Revision. Use **scale to zero** for the transition and **cold** for the state; drop "idle" as a state name.
- **"Target"** is overloaded: a row in the **Traffic table** and the **Autoscaler**'s per-replica concurrency goal. Say **traffic target** and **concurrency target**.
- **"Namespace"** unqualified caused a real bug (the endpoint flip looked for Activator pods in the wrong one). Always say **Workload namespace** or **Release namespace**.
- **"Request"** names the client-supplied job/deployment spec but also every HTTP request on the data path. Prefer "spec" in serving-plane conversation and keep **Request** for the jobs API payload.
- **"Event"** is claimed three ways: **CloudEvent** (callback envelope), **Signal** (internal lifecycle), and Kubernetes watch events. The first two are canonical; call the third "watch events".
