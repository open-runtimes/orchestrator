# Workload log shipping

The Kubernetes chart can run one node-local log collector on every selected
Linux node. It ships user workload `stdout` and `stderr` for Jobs, Deployments,
and Sandboxes to any OTLP/HTTP logs endpoint.

The collector is opt-in and uses Grafana Alloy. It reads CRI files directly
from the node's `/var/log/pods`; it does not proxy bytes through an
orchestrator service or open one Kubernetes API log stream per workload. A
node-scoped Pod watch supplies identity labels. Only the user containers are
selected:

| Workload | Containers |
| --- | --- |
| Job | `worker` |
| Deployment | `worker` for direct replicas, `workload` for warm claims |
| Sandbox | `workload` |

Init containers, workload sidecars, unclaimed warm pods, and control-plane
pods are excluded. Warm claims are safe to collect from their first user byte:
the claim protocol stamps final identity labels before activating the
workload.

## Enable it

An OTLP base endpoint is required. Alloy sends protobuf requests to
`<endpoint>/v1/logs`.

```yaml
logCollector:
  enabled: true
  clusterName: production-eu
  otlp:
    endpoint: https://otel.example.com
```

For an authenticated endpoint, create a Secret in the Helm release namespace.
The value is used verbatim as the HTTP `Authorization` header, so include its
scheme:

```bash
kubectl -n orchestrator create secret generic workload-logs \
  --from-literal=authorization='Bearer replace-me'
```

```yaml
logCollector:
  enabled: true
  otlp:
    endpoint: https://otel.example.com
    auth:
      existingSecret: workload-logs
      secretKey: authorization
```

Credentials are mounted from the Secret and watched for changes, so rotation
does not require a collector restart. They are never written into the generated
Alloy ConfigMap.

## Record attributes

CRI timestamps and the `stdout`/`stderr` stream are preserved. Each OTLP log
record carries the applicable attributes below:

| Attribute | Meaning |
| --- | --- |
| `orchestrator_workload_kind` | `job`, `deployment`, or `sandbox` |
| `orchestrator_job_id` | Job identity |
| `orchestrator_deployment_id` | Deployment identity |
| `orchestrator_deployment_revision` | Immutable Revision identity |
| `orchestrator_sandbox_id` | Sandbox identity; never the capability token |
| `k8s_cluster_name` | Operator-supplied cluster name, when configured |
| `k8s_namespace_name` | Pod namespace |
| `k8s_node_name` | Node name |
| `k8s_pod_name`, `k8s_pod_uid` | Pod identity and incarnation |
| `k8s_container_name` | Source container |
| `stream` | `stdout` or `stderr` |

The collector copies only this allowlist. In particular,
`sandbox.token`, callback keys, annotations, and arbitrary workload labels are
not exported.

## Delivery and storage

Alloy stores file positions and its OTLP retry queue below a release-specific
host path, `/var/lib/orchestrator/<release>/log-collector` by default. The
queue is fsynced, blocks on overflow, and retries indefinitely. This gives
at-least-once delivery across collector restarts while the node disk survives;
receivers must tolerate duplicates. Node loss and kubelet deletion of unread
rotated files remain loss boundaries.

The host directory is transient transport state, not a log archive. Successfully
sent queue entries and positions for disappeared targets are reclaimed by
Alloy. `storageHostPath`, queue size, batch size, and flush interval are all
configurable under `logCollector`.

The DaemonSet runs as root because kubelet log files and a host-created state
directory are not portably readable/writable by an arbitrary UID. It has a
read-only root filesystem, all Linux capabilities dropped, no host PID/network
access, and only these host mounts:

- `/var/log/pods`, read-only;
- its release-specific state directory, read-write.

Its ServiceAccount receives `get`, `list`, and `watch` on Pods only, through
namespace Roles for the release, Job, and workload namespaces. It cannot read
Secrets through the Kubernetes API or call `pods/log`.

## Relationship to Job callbacks

OTLP shipping is independent from `orchestrator.job.log` callbacks. Enabling
the collector does not change callback behavior in this first release. Once
the node pipeline has production parity, the Kubernetes Job watcher's API-based
log streamer can be removed separately without coupling external log storage
to lifecycle callbacks.
