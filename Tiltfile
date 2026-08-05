# -*- mode: Python -*-
#
# Live-reload dev loop for the orchestrator Helm chart.
# Usage: `task kind:up && tilt up`.
#
# Tilt watches Go source, rebuilds images with ko, tags them to the refs Helm
# expects, and applies the chart to the local kind cluster.

# Guard against accidental deploys to anything other than the local dev cluster.
allow_k8s_contexts('kind-orchestrator-dev')

# Opt-in serving plane (Phase 0 skeleton): `tilt up -- --deployments`.
config.define_bool('deployments')
DEPLOYMENTS_ENABLED = config.parse().get('deployments', False)

JOBS_SERVICE_IMAGE        = 'ko.local/jobs-service'
JOB_SIDECAR_IMAGE         = 'ko.local/job-sidecar'
DEPLOYMENTS_SERVICE_IMAGE = 'ko.local/deployments-service'
WORKLOAD_SIDECAR_IMAGE = 'ko.local/workload-sidecar'
DEPLOYMENTS_ACTIVATOR_IMAGE = 'ko.local/deployments-activator'
SANDBOX_PROXY_IMAGE = 'ko.local/sandbox-proxy'
POOL_SHIM_IMAGE = 'ko.local/pool-shim'

# --- Image builds ------------------------------------------------------------

# Build the jobs-service image with ko, then tag it to the ref Tilt injects.
custom_build(
    JOBS_SERVICE_IMAGE,
    'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/jobs-service && docker tag {0} $EXPECTED_REF'.format(JOBS_SERVICE_IMAGE),
    deps=[
        'cmd/jobs-service',
        'internal',
        'pkg',
        'go.mod',
        'go.sum',
    ],
)

custom_build(
    JOB_SIDECAR_IMAGE,
    'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/job-sidecar && docker tag {0} $EXPECTED_REF'.format(JOB_SIDECAR_IMAGE),
    deps=[
        'cmd/job-sidecar',
        'internal',
        'pkg',
        'go.mod',
        'go.sum',
    ],
)

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
if DEPLOYMENTS_ENABLED:
    custom_build(
        DEPLOYMENTS_SERVICE_IMAGE,
        'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/deployments-service && docker tag {0} $EXPECTED_REF'.format(DEPLOYMENTS_SERVICE_IMAGE),
        deps=[
            'cmd/deployments-service',
            'internal',
            'pkg',
            'go.mod',
            'go.sum',
        ],
    )
    custom_build(
        WORKLOAD_SIDECAR_IMAGE,
        'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/workload-sidecar && docker tag {0} $EXPECTED_REF'.format(WORKLOAD_SIDECAR_IMAGE),
        deps=[
            'cmd/workload-sidecar',
            'internal/proxy',
            'go.mod',
            'go.sum',
        ],
    )
    custom_build(
        DEPLOYMENTS_ACTIVATOR_IMAGE,
        'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/deployments-activator && docker tag {0} $EXPECTED_REF'.format(DEPLOYMENTS_ACTIVATOR_IMAGE),
        deps=[
            'cmd/deployments-activator',
            'internal',
            'go.mod',
            'go.sum',
        ],
    )
    custom_build(
        SANDBOX_PROXY_IMAGE,
        'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/sandbox-proxy && docker tag {0} $EXPECTED_REF'.format(SANDBOX_PROXY_IMAGE),
        deps=[
            'cmd/sandbox-proxy',
            'internal',
            'go.mod',
            'go.sum',
        ],
    )
    custom_build(
        POOL_SHIM_IMAGE,
        'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/pool-shim && docker tag {0} $EXPECTED_REF'.format(POOL_SHIM_IMAGE),
        deps=[
            'cmd/pool-shim',
            'internal/proxy',
            'go.mod',
            'go.sum',
        ],
    )
    helm_set = [
        'deployments.enabled=true',
        'deployments.activator.enabled=true',
        'deployments.image.repository=' + DEPLOYMENTS_SERVICE_IMAGE,
        'deployments.image.pullPolicy=Never',
        'workloadSidecarImage.repository=' + WORKLOAD_SIDECAR_IMAGE,
        'deployments.activator.image.repository=' + DEPLOYMENTS_ACTIVATOR_IMAGE,
        'deployments.activator.image.pullPolicy=Never',
        'deployments.shimImage.repository=' + POOL_SHIM_IMAGE,
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
