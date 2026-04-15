# resource-metrics e2e suite

Chainsaw scenarios against a kind cluster running milo + the test-infra
observability stack + the resource-metrics controller.

## Prerequisites

```sh
task test:setup    # kind up, milo, observability (VM + OTel), controller
export KUBECONFIG=.test-infra/kubeconfig   # or whatever task:setup emits
```

`task test:setup` performs `test-infra:cluster-up`, installs milo,
installs the observability stack, and deploys resource-metrics. The
suite assumes all of that is already in place.

## Running

```sh
task e2e                               # whole suite, sequential
./bin/chainsaw test test/e2e/policy-lifecycle --config test/e2e/.chainsaw.yaml
```

## Layout

```
test/e2e/
├── .chainsaw.yaml          runner config (parallel:1, shared timeouts)
├── fixtures/               shared YAML (Org, Project, aggregation CR)
├── policy-lifecycle/       Ready condition, activeGenerators, re-apply
├── invalid-cel/            InvalidCEL status path
├── rbac-denied/            PermissionDenied + missingPermissions
├── partial-compile/        one valid + one broken generator -> Degraded
└── metrics-emission/       series actually lands in Victoria Metrics
```

## Shared-fixtures model

Every test creates (or re-applies idempotently) `e2e-org`, `e2e-alpha`,
and the aggregation `ClusterRole` on the project control plane in its
setup block. Cleanup only removes each test's own `ResourceMetricsPolicy`,
not the fixtures, so subsequent tests start with the Project already
Ready.

## Multi-cluster note

Each test's setup writes `./.project-cp.kubeconfig` by cloning the
current kubeconfig and rewriting the server URL to milo's
`/apis/resourcemanager.miloapis.com/v1alpha1/projects/e2e-alpha/control-plane`
aggregation path. That file is referenced as `project-cp` in the test's
`clusters:` block so individual `apply:` / `assert:` / `delete:` steps
can retarget via `cluster: project-cp`. Management-cluster operations
use the default kubeconfig unchanged.

### Warning: isolate `KUBECONFIG` before running

The setup scripts run `kubectl config view --raw --flatten` against
`$KUBECONFIG` and rewrite every `server:` line. If `$KUBECONFIG` points at
a merged config containing unrelated contexts (a typical developer laptop),
the generated project-CP kubeconfig will have every cluster's server URL
rewritten to the milo aggregation path — unusable for anything except this
test run, and likely confusing if you then re-point `$KUBECONFIG` at it.

`task e2e` already exports `KUBECONFIG=.test-infra/kubeconfig` before
invoking chainsaw, so the default flow is safe. If you run chainsaw
directly, set `KUBECONFIG` yourself:

```sh
KUBECONFIG=.test-infra/kubeconfig ./bin/chainsaw test test/e2e/...
```
