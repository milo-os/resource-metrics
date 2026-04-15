# resource-metrics

A Kubernetes operator that emits custom resource state metrics from Milo
project control planes as OTLP. It replaces per-project
kube-state-metrics deployments with a single, centrally-operated
controller that watches every project control plane via the Milo
multi-cluster provider and pushes metrics to the platform's OpenTelemetry
collector.

See the [Datum Cloud docs](https://docs.datum.net) and
[`docs/README.md`](docs/README.md) for the design.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
