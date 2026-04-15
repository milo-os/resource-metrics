# e2e fixtures

Shared fixtures for chainsaw scenarios against a kind cluster running
milo's `overlays/test-infra` and resource-metrics in
`discovery.mode: milo` with `internalServiceDiscovery: false`.

## Target kind: core/v1 ConfigMap

Chosen for minimal blast radius: `ConfigMap` is served by every project
control plane with no extra CRD bootstrap. Milo's Project controller
only ensures a `default` Namespace plus optional Gateway/DNS classes
(see `milo/internal/controllers/resourcemanager/project_controller.go:243`),
so any custom CRD would need its own per-project install mechanism that
doesn't exist today. Richer kinds (e.g. `compute.miloapis.com/Workload`)
are **not** guaranteed to be installed in the project CP.

## Files

| File | Target cluster | Purpose |
|---|---|---|
| `organization.yaml` | management | parent of `Project` |
| `project.yaml` | management | creates the `e2e-alpha` project CP |
| `aggregation-labels.yaml` | project CP (`e2e-alpha`) | `ClusterRole` labeled for RBAC aggregation so the controller can list/watch `configmaps` |
| `kustomization.yaml` | — | validation only (`kubectl kustomize`) |

## Order of operations (chainsaw scenarios)

1. Apply `organization.yaml` and `project.yaml` to the management cluster.
2. Wait for `Project/e2e-alpha` `status.conditions[type=Ready].status=True`.
3. Apply `aggregation-labels.yaml` to the project CP via the aggregation
   path
   `{{milo-apiserver}}/apis/resourcemanager.miloapis.com/v1alpha1/projects/e2e-alpha/control-plane`.
4. Apply the scenario-specific `ResourceMetricsPolicy` + target ConfigMaps.
