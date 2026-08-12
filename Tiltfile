# -*- mode: Python -*-
#
# Live-reload dev loop for the orchestrator Helm chart.
# Usage: `task kind:up && tilt up`.
#
# Tilt watches Go source, rebuilds images with ko, tags them to the refs Helm
# expects, and applies the chart to the local kind cluster.

# Guard against accidental deploys to anything other than the local dev cluster.
allow_k8s_contexts('kind-orchestrator-dev')

# Opt-in planes: `tilt up -- --deployments --sandboxes`.
config.define_bool('deployments')
config.define_bool('sandboxes')
DEPLOYMENTS_ENABLED = config.parse().get('deployments', False)
SANDBOXES_ENABLED = config.parse().get('sandboxes', False)

JOBS_SERVICE_IMAGE        = 'ko.local/jobs-service'
JOB_SIDECAR_IMAGE         = 'ko.local/job-sidecar'
DEPLOYMENTS_SERVICE_IMAGE = 'ko.local/deployments-service'
SANDBOXES_SERVICE_IMAGE = 'ko.local/sandboxes-service'
WORKLOAD_SIDECAR_IMAGE = 'ko.local/workload-sidecar'
DEPLOYMENTS_ACTIVATOR_IMAGE = 'ko.local/deployments-activator'
SANDBOX_PROXY_IMAGE = 'ko.local/sandbox-proxy'
POOL_SHIM_IMAGE = 'ko.local/pool-shim'

# --- Image builds ------------------------------------------------------------

def ko_build(image, package, deps):
    custom_build(
        image,
        'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 {1} && docker tag {0} $EXPECTED_REF'.format(image, package),
        deps=deps + ['go.mod', 'go.sum'],
    )

ko_build(JOBS_SERVICE_IMAGE, './cmd/jobs-service', ['cmd/jobs-service', 'internal'])
ko_build(JOB_SIDECAR_IMAGE, './cmd/job-sidecar', ['cmd/job-sidecar', 'internal'])

# --- Namespace ---------------------------------------------------------------
# The chart is deliberately namespace-agnostic — create it here for dev.

k8s_yaml(blob('''
apiVersion: v1
kind: Namespace
metadata:
  name: orchestrator
'''))

# --- Helm release ------------------------------------------------------------

helm_set = []

# The workload-sidecar and pool-shim ride in every serving workload's pod, for
# both planes.
if DEPLOYMENTS_ENABLED or SANDBOXES_ENABLED:
    ko_build(WORKLOAD_SIDECAR_IMAGE, './cmd/workload-sidecar', ['cmd/workload-sidecar', 'internal'])
    ko_build(POOL_SHIM_IMAGE, './cmd/pool-shim', ['cmd/pool-shim', 'internal'])
    helm_set += [
        'workloadSidecarImage.repository=' + WORKLOAD_SIDECAR_IMAGE,
        'deployments.shimImage.repository=' + POOL_SHIM_IMAGE,
    ]

if DEPLOYMENTS_ENABLED:
    ko_build(DEPLOYMENTS_SERVICE_IMAGE, './cmd/deployments-service', ['cmd/deployments-service', 'internal'])
    ko_build(DEPLOYMENTS_ACTIVATOR_IMAGE, './cmd/deployments-activator', ['cmd/deployments-activator', 'internal'])
    helm_set += [
        'deployments.enabled=true',
        'deployments.activator.enabled=true',
        'deployments.image.repository=' + DEPLOYMENTS_SERVICE_IMAGE,
        'deployments.image.pullPolicy=Never',
        'deployments.activator.image.repository=' + DEPLOYMENTS_ACTIVATOR_IMAGE,
        'deployments.activator.image.pullPolicy=Never',
    ]

if SANDBOXES_ENABLED:
    ko_build(SANDBOXES_SERVICE_IMAGE, './cmd/sandboxes-service', ['cmd/sandboxes-service', 'internal'])
    ko_build(SANDBOX_PROXY_IMAGE, './cmd/sandbox-proxy', ['cmd/sandbox-proxy', 'internal'])
    helm_set += [
        'sandboxes.enabled=true',
        'sandboxes.domain=sandboxes.localhost',
        'sandboxes.image.repository=' + SANDBOXES_SERVICE_IMAGE,
        'sandboxes.image.pullPolicy=Never',
        'sandboxes.proxy.enabled=true',
        'sandboxes.proxy.image.repository=' + SANDBOX_PROXY_IMAGE,
        'sandboxes.proxy.image.pullPolicy=Never',
    ]

k8s_yaml(helm(
    'charts/orchestrator',
    name='orchestrator',
    namespace='orchestrator',
    values=['hack/dev-values.yaml'],
    set=helm_set,
))

k8s_resource(
    'jobs',
    port_forwards=[
        port_forward(8080, 8080, name='api'),
        port_forward(9090, 9090, name='metrics'),
    ],
    labels=['orchestrator'],
    objects=[
        'orchestrator:namespace',
        'jobs:serviceaccount',
        'job-sidecar:serviceaccount',
        'jobs:role',
        'jobs:rolebinding',
    ],
    links=[
        link('http://localhost:8080/readyz', 'readyz'),
        link('http://localhost:9090/metrics', 'metrics'),
    ],
)

if DEPLOYMENTS_ENABLED:
    k8s_resource(
        'deployments',
        port_forwards=[
            port_forward(8081, 8080, name='api'),
            port_forward(9091, 9090, name='metrics'),
        ],
        labels=['orchestrator'],
        links=[
            link('http://localhost:8081/readyz', 'readyz'),
            link('http://localhost:9091/metrics', 'metrics'),
        ],
    )

if SANDBOXES_ENABLED:
    k8s_resource(
        'sandboxes',
        port_forwards=[
            port_forward(8082, 8080, name='api'),
            port_forward(9092, 9090, name='metrics'),
        ],
        labels=['orchestrator'],
        links=[
            link('http://localhost:8082/readyz', 'readyz'),
            link('http://localhost:9092/metrics', 'metrics'),
        ],
    )
