# -*- mode: Python -*-
#
# Live-reload dev loop for the orchestrator Helm chart.
# Usage: `task kind:up && tilt up`.
#
# Tilt watches Go source, rebuilds images with ko, tags them to the refs Helm
# expects, and applies the chart to the local kind cluster.

# Guard against accidental deploys to anything other than the local dev cluster.
allow_k8s_contexts('kind-orchestrator-dev')

JOBS_SERVICE_IMAGE = 'ko.local/jobs-service'
SIDECAR_IMAGE      = 'ko.local/job-sidecar'

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
    SIDECAR_IMAGE,
    'KO_DOCKER_REPO={0} ./bin/ko build --bare --platform=linux/amd64 ./cmd/job-sidecar && docker tag {0} $EXPECTED_REF'.format(SIDECAR_IMAGE),
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

k8s_yaml(helm(
    'charts/orchestrator',
    name='orchestrator',
    namespace='orchestrator',
    values=['hack/dev-values.yaml'],
))

k8s_resource(
    'orchestrator',
    port_forwards=[
        port_forward(8080, 8080, name='api'),
        port_forward(9090, 9090, name='metrics'),
    ],
    labels=['orchestrator'],
    objects=[
        'orchestrator:namespace',
        'orchestrator:serviceaccount',
        'job-sidecar:serviceaccount',
        'orchestrator:role',
        'orchestrator:rolebinding',
    ],
    links=[
        link('http://localhost:8080/readyz', 'readyz'),
        link('http://localhost:9090/metrics', 'metrics'),
    ],
)
