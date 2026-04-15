# Resource Metrics

**Status:** Draft

## Summary

Resource Metrics is the platform service that collects and emits state
metrics for custom resources across all Milo project control planes. It
replaces a per-project kube-state-metrics deployment with a single,
centrally-operated controller that watches every project control plane
via the Milo multi-cluster provider and pushes metrics to the platform's
Victoria Metrics cluster over OTLP.

## The Problem

Each Datum project runs its own dedicated Kubernetes API server — a
virtual control plane managed by Milo. The platform needs to collect
state metrics from the custom resources living in those control planes
(workloads, network services, DNS zones, and so on) so that operators
can build dashboards, fire alerts, and understand platform-wide resource
health.

Today, this is solved by deploying one kube-state-metrics (KSM) pod per
project control plane. When a `ProjectControlPlane` resource is created,
a Kyverno policy generates a Flux Kustomization that deploys a KSM
instance into the project's namespace, pointed at that project's virtual
API server via a mTLS kubeconfig. A kiwigrid sidecar watches for
ConfigMaps carrying `CustomResourceStateMetrics` configuration and
hot-reloads KSM when they appear.

This approach has several problems that compound as the number of
projects grows:

- **Linear resource growth.** Every new project means one more KSM pod,
  one more cert, one more kubeconfig ConfigMap, one more ClusterRoleBinding,
  and one more PodMonitor. There is no sharing between projects and no
  upper bound.

- **Spot preemption instability.** KSM pods running on spot nodes are
  evicted every few hours. Each eviction rotates the pod's identity
  labels (`instance`, `pod`), which breaks alert fingerprints and
  triggers spurious Slack notifications until the new pod is scraped.
  Pinning KSM to non-spot node pools trades cost efficiency for
  stability — and requires a dedicated node pool that must itself be
  sized for the worst-case project count.

- **VMAgent scrape fanout.** Every project adds discovery entries and
  scrape targets to VMAgent. At hundreds of projects this becomes a
  meaningful source of latency and memory pressure in the scrape pipeline
  itself, independent of KSM's own footprint.

- **Fragile wiring.** Monitoring is connected to a project solely
  through a Kyverno policy that fires once at creation. There is no
  ongoing reconciliation. If the policy misfires, or if a project is
  renamed or deleted, the monitoring state can drift silently.

- **Sidecar complexity.** The kiwigrid sidecar that compiles
  `CustomResourceStateMetrics` ConfigMaps adds a second process, a
  second failure mode, and a second image to update to every project's
  monitoring pod.

None of these problems is fatal at current project counts. All of them
become worse as the platform grows.

## The Solution

Resource Metrics is a single controller that replaces all per-project
KSM deployments. It runs centrally, discovers project control planes
through the Milo multi-cluster provider, and watches the custom
resources registered with it across all of those control planes
simultaneously. Instead of exposing a Prometheus `/metrics` endpoint for
scraping, it pushes metrics over OTLP directly to an OpenTelemetry
collector, which forwards them to Victoria Metrics via remote write.

The key idea: operators define what to emit once, in a cluster-scoped
`ResourceMetricsPolicy` resource, and the controller applies that
definition to every project control plane — including control planes that
are created after the policy is written.

## How It Works

```
┌─────────────────────────────────────────────────────────────────┐
│                     Management Cluster                           │
│                                                                  │
│   ResourceMetricsPolicy  ──►  resource-metrics controller        │
│   (cluster-scoped config)      (multicluster-runtime manager)   │
│                                          │                       │
│                                          │  Milo provider        │
└──────────────────────────────────────────┼───────────────────────┘
                                           │ discovers & watches
                          ┌────────────────┼────────────────┐
                          │                │                │
                          ▼                ▼                ▼
                   ┌────────────┐  ┌────────────┐  ┌────────────┐
                   │ Project CP │  │ Project CP │  │ Project CP │
                   │  alpha     │  │  beta      │  │  gamma     │
                   │            │  │            │  │            │
                   │ Workloads  │  │ Workloads  │  │ Workloads  │
                   │ Networks   │  │ Networks   │  │ Networks   │
                   └────────────┘  └────────────┘  └────────────┘
                          │                │                │
                          └────────────────┴────────────────┘
                                           │ OTLP push
                                           ▼
                              ┌─────────────────────┐
                              │   OTel Collector     │
                              │   Gateway            │
                              └──────────┬──────────┘
                                         │ remote write
                                         ▼
                              ┌─────────────────────┐
                              │   Victoria Metrics   │
                              └─────────────────────┘
```

When a new project control plane is engaged by the Milo provider, the
controller starts informers for every resource kind registered in active
`ResourceMetricsPolicy` objects. It uses the OTel SDK's async gauge
instruments, re-evaluating the full set of live objects on each
collection cycle. When a resource is deleted, it naturally falls off the
next cycle and goes stale in Victoria Metrics within the staleness
window. No explicit tombstoning is required.

Each metric series carries the project name and project UID as
attributes, which are promoted to Prometheus labels by the remote write
exporter. This preserves compatibility with existing dashboards and
PromQL queries.

## The API

`ResourceMetricsPolicy` is a cluster-scoped resource under
`resourcemetrics.miloapis.com/v1alpha1`. It applies to all project
control planes — there is no per-project selector. If a resource kind
specified in a generator does not exist on a given control plane, the
informer returns nothing and no series are emitted for that control
plane.

### ResourceMetricsPolicy

```yaml
apiVersion: resourcemetrics.miloapis.com/v1alpha1
kind: ResourceMetricsPolicy
metadata:
  name: compute-metrics
spec:
  generators:
    - name: workload-info
      resource:
        group: compute.miloapis.com
        version: v1alpha1
        resource: workloads
      families:
        - name: workload_info
          help: "Exposes metadata about each Workload resource."
          type: gauge
          metrics:
            - labels:
                - name: name
                  value: "object.metadata.name"
                - name: namespace
                  value: "object.metadata.namespace"
                - name: phase
                  value: "object.status.phase"

    - name: workload-ready-condition
      resource:
        group: compute.miloapis.com
        version: v1alpha1
        resource: workloads
      families:
        - name: workload_status_condition
          help: "Condition status for each Workload."
          type: gauge
          metrics:
            - value: "object.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True') ? 1.0 : 0.0"
              labels:
                - name: name
                  value: "object.metadata.name"
                - name: namespace
                  value: "object.metadata.namespace"
                - name: condition
                  value: "'Ready'"
```

Each `generator` targets one Kubernetes API resource (identified by
group, version, and plural resource name) and defines one or more metric
families to emit per object. Metric values and label values are CEL
expressions evaluated against the resource object. An omitted `value`
defaults to `1.0`.

### Design Principles

**CEL only.** Label and value expressions use CEL exclusively. This
keeps the expression language consistent with the rest of the Datum
platform, avoids shipping a Starlark interpreter, and limits the attack
surface of user-supplied expressions.

**One policy per concern.** `ResourceMetricsPolicy` objects are
cluster-scoped singletons keyed by name. Platform teams define one
policy per domain (`compute-metrics`, `network-metrics`, `dns-metrics`)
rather than one monolithic policy for everything. This makes ownership
clear and allows independent updates.

**Derived from upstream.** The `GeneratorSpec` shape is structurally
aligned with `kubernetes-sigs/resource-state-metrics` (KEP-4785,
SIG Instrumentation). We own the API surface and the API group but
borrow the vocabulary — generator, family, metric, label — so the model
is recognizable to anyone who has worked with KSM's
`CustomResourceStateMetrics`.

## Comparison with Current Architecture

| Concern | Current (per-project KSM) | Resource Metrics |
|---|---|---|
| Deployment footprint | 1 pod + 1 sidecar per project | 1 controller for all projects |
| Discovery | Kyverno fires once at creation | Milo provider reconciles continuously |
| Metric pipeline | Pull (VMAgent scrapes PodMonitor) | Push (OTLP to collector gateway) |
| CRD config surface | KSM `CustomResourceStateMetrics` YAML in ConfigMaps | `ResourceMetricsPolicy` CRD |
| Expression language | JSONPath + KSM DSL | CEL |
| Spot sensitivity | High (pod eviction breaks alert fingerprints) | None (no per-project pods) |
| New project onboarding | Kyverno generates new pod, cert, RBAC | Milo provider engages new cluster automatically |

## Relationship to Other Platform Services

**telemetry-services-operator** manages user-facing telemetry export —
it lets customers send their own metrics and logs to external
observability systems. Resource Metrics is entirely separate: it is
platform self-observation, not customer-facing, and it pushes to the
platform's own Victoria Metrics rather than to customer-defined sinks.
The two services share the `miloapis.com` API domain and the
multicluster-runtime + Milo provider pattern, but they have different
API groups, different RBAC surfaces, and different failure domains.

**kube-state-metrics** (the core deployment in `telemetry-system`)
continues to serve metrics for standard Kubernetes resource types
(Deployments, Pods, Nodes, etc.) on the management cluster. Resource
Metrics does not replace it; it adds coverage for Datum custom resources
on project control planes.

## Migration

The existing per-project KSM deployments and Resource Metrics can run
side by side during migration. The new service emits metrics under its
own naming conventions; there is no collision with the existing
`kube_customresource_*` series.

Recommended sequence:

1. Deploy Resource Metrics with a small set of generators covering
   high-value CRDs.
2. Validate series parity against the existing KSM output in staging for
   one retention window.
3. Update recording rules and dashboards to reference the new series.
4. Remove the Kyverno policy that generates per-project KSM deployments.
5. Allow existing per-project KSM pods to drain naturally as their
   projects are updated or recycled.

## What This Is Not

**Not a replacement for core KSM.** Standard Kubernetes types
(Deployments, Pods, Nodes) continue to be served by the existing
kube-state-metrics deployment in `telemetry-system`. Resource Metrics
only covers Datum custom resources on project control planes.

**Not a user-facing telemetry service.** Customers cannot configure
Resource Metrics or read its output directly. It is an internal platform
observability component.

**Not a general-purpose OTLP pipeline.** Resource Metrics emits
Kubernetes resource state metrics. General-purpose log and trace
collection is handled by Vector, Loki, and Tempo.

## Open Questions

- How do we handle CEL expression errors at evaluation time — skip the
  series silently, emit a sentinel value, or surface an error condition
  on the policy status?
- What is the right staleness window configuration in Victoria Metrics
  for push-based series, and how do we make it discoverable to dashboard
  authors?
- Should `ResourceMetricsPolicy` support a `namespaceSelector` within a
  project control plane (to scope generators to specific namespaces), or
  is cluster-wide always correct for our CRDs?
- How do we handle a project control plane that is temporarily
  unreachable — emit a sentinel `resource_metrics_project_up` series,
  suppress all series, or both?
- What RBAC does the controller need on each project control plane, and
  how is that provisioned as new CRDs are registered in new policies?
