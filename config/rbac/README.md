# RBAC

Resource-metrics has two distinct RBAC surfaces, deployed to two different
clusters.

## Management cluster — `role.yaml`

The ClusterRole in `role.yaml` is generated from `// +kubebuilder:rbac:`
markers on the reconciler (`internal/controller/`). It is applied on the
**management cluster** and grants the controller the permissions it needs
to:

- read / watch / patch `ResourceMetricsPolicy` objects and their status
  subresource, and
- issue `SelfSubjectAccessReview` requests (used by each per-project
  informer's pre-flight against its project CP).

Do not hand-edit `role.yaml`; run `make manifests` to regenerate it from
the markers.

## Per-project CP — `aggregated-project-role.yaml`

The ClusterRole in `aggregated-project-role.yaml` is deployed **per project
control plane**, not on the management cluster. It is an aggregated role:
its rules are empty, and the Kubernetes RBAC aggregator fills them from
every ClusterRole that carries

```
resourcemetrics.miloapis.com/aggregate-to-reader: "true"
```

CRD owners who want resource-metrics to be able to observe their resources
opt in by labelling their own project-scoped reader ClusterRoles with that
label. The project-CP bootstrap is responsible for binding the resulting
aggregated role to the controller's service account via a
ClusterRoleBinding.

`aggregated-project-role.yaml` is deliberately excluded from the management
cluster `kustomization.yaml` so `make deploy` does not try to install it on
the wrong cluster.
