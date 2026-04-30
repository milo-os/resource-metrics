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

## Root control plane — `collectRootControlPlane: true`

When `discovery.collectRootControlPlane: true` is set in the server config,
the operator also collects metrics from the management/root cluster itself
(the same cluster where `ResourceMetricsPolicy` objects live). Metrics from
this cluster are emitted with the attribute `milo.project.name = "root"`.

The aggregated-role mechanism that covers project CPs does **not** apply to
the root cluster. You must deploy explicit RBAC there for each resource type
your policies target.

The operator's ServiceAccount on the management cluster is
`resource-metrics-controller-manager` in namespace `resource-metrics-system`
(subject to your kustomize namespace/namePrefix overrides).

### Example

To allow the operator to list and watch `Project` resources from
`resourcemanager.miloapis.com` on the root cluster:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: resource-metrics-root-reader
rules:
- apiGroups: ["resourcemanager.miloapis.com"]
  resources: ["projects"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: resource-metrics-root-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: resource-metrics-root-reader
subjects:
- kind: ServiceAccount
  name: resource-metrics-controller-manager
  namespace: resource-metrics-system
```

Apply this manifest to the management cluster before or alongside the
operator deployment. Add additional rules for each resource type you want
to collect from the root control plane.

If permissions are missing, the affected GVRs will appear in the
`missingPermissions` field and a `PermissionDenied=True` condition will be
set on the `ResourceMetricsPolicy` status.
