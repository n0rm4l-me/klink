# klink

Kubernetes operator for workload dependency management. Automatically scales dependent services to zero when their dependencies become unhealthy, and restores them when dependencies recover.

[![Tests](https://github.com/n0rm4l-me/klink/actions/workflows/test.yml/badge.svg)](https://github.com/n0rm4l-me/klink/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

## Supported workload types

| Kind | As dependent | As dependency | Notes |
|------|-------------|--------------|-------|
| `Deployment` | ✅ scale to 0 | ✅ readyReplicas check | |
| `StatefulSet` | ✅ scale to 0 | ✅ readyReplicas check | |
| `CronJob` | ✅ suspend=true | — | no replicas concept |
| `Rollout` (Argo) | ✅ scale to 0 | ✅ phase check | suspension deferred during active canary |

## How it works

```yaml
apiVersion: deps.klink.dev/v1alpha1
kind: WorkloadDependency
metadata:
  name: payments-needs-database
  namespace: my-app
spec:
  dependent:
    kind: Rollout        # Deployment | StatefulSet | CronJob | Rollout
    name: payments

  dependsOn:
    - kind: Deployment
      name: database
      condition:
        minReadyPercent: 80   # healthy if ≥80% replicas ready
        window: 30s            # wait 30s before acting (hysteresis)
        recoveryWindow: 60s    # wait 60s after recovery before restoring

  onDegraded:
    action: ScaleToZero

  mode: strict  # strict | soft | gate
```

When `database` becomes unhealthy:
1. klink waits for `window` (hysteresis — ignores transient restarts)
2. Scales `payments` to 0, saving its replica count
3. When `database` recovers, waits for `recoveryWindow`, then restores `payments`

**CronJob** dependents: sets `spec.suspend=true` instead of scaling. Resumes on recovery.

**Rollout** dependents: suspension is deferred if a canary/blue-green rollout is in progress — klink never interrupts an active deployment.

## Enforcement modes

| Mode | Behavior |
|------|----------|
| `strict` | Re-enforces scale-to-zero on every reconcile while dependency is down. Manual scale-up reverted within 15s. |
| `soft` | Scales to zero once but does not fight manual changes. |
| `gate` | Blocks scale-up via admission webhook while dependency is unhealthy. Does not scale to zero. |

## Status

```
kubectl get workloaddependencies -A

NAMESPACE  NAME                      PHASE       REPLICAS   MESSAGE                               AGE
my-app     payments-needs-database   Suspended   3          dependency database not healthy        5m
my-app     billing-needs-database    Suspended              dependency database not healthy         5m
```

| Phase | Meaning |
|-------|---------|
| `Healthy` | All dependencies healthy |
| `Degraded` | Dependency unhealthy, within hysteresis window |
| `Suspended` | Dependent scaled to zero (or CronJob suspended) |
| `Paused` | Enforcement disabled via `klink.dev/paused` annotation |
| `Unknown` | Dependent workload not found |

## Mutual dependencies

klink handles A→B + B→A without deadlock. When klink scales a service to zero, other `WorkloadDependency` objects that depend on it recognize it as `CoSuspended` and don't cascade.

When you manually restore one service, klink automatically restores the other.

## Pausing

```bash
# Pause enforcement
kubectl annotate workloaddependency payments-needs-database klink.dev/paused=true

# Resume
kubectl annotate workloaddependency payments-needs-database klink.dev/paused-
```

## Installation

```bash
helm upgrade --install klink oci://ghcr.io/n0rm4l-me/charts/klink \
  --namespace klink-system \
  --create-namespace
```

Or from source:

```bash
helm upgrade --install klink ./charts/klink \
  --namespace klink-system \
  --create-namespace \
  --set image.tag=0.2.0
```

## Metrics

klink exposes Prometheus metrics at `:8080/metrics` (enabled by default):

| Metric | Type | Description |
|--------|------|-------------|
| `klink_dependency_phase` | Gauge | Current phase per WD (1=active) |
| `klink_scale_to_zero_total` | Counter | Scale-to-zero actions |
| `klink_replicas_restored_total` | Counter | Replica restore actions |
| `klink_reconcile_errors_total` | Counter | Reconciliation errors |
| `klink_suspended_workloads` | Gauge | Currently suspended workloads |

GKE Managed Prometheus: `PodMonitoring` resource is created automatically when `metrics.enabled=true`.

## Configuration reference

```yaml
spec:
  dependent:
    kind: Deployment | StatefulSet | CronJob | Rollout
    name: string
    namespace: string     # optional, defaults to WorkloadDependency namespace

  dependsOn:
    - kind: Deployment | StatefulSet | Rollout   # CronJob not supported as dependency
      name: string
      namespace: string   # optional, cross-namespace supported
      condition:
        minReadyPercent: 100   # 0-100, default 100
        window: 30s            # default 30s
        recoveryWindow: 60s    # default 60s

  onDegraded:
    action: ScaleToZero

  mode: strict | soft | gate   # default strict
```

## k9s plugins

Copy `contrib/k9s-plugins.yaml` entries into `~/.config/k9s/plugins.yaml`.

| Shortcut | Action |
|----------|--------|
| `d` | Describe WorkloadDependency |
| `Ctrl-E` | Show events |
| `Ctrl-L` | Operator logs |
| `Ctrl-S` | Show dependent workload |
| `Ctrl-F` | Force reconcile |

## Documentation

- [Architecture](docs/architecture.md) — component diagram, state machine, sequence diagrams
- [Core Concepts](docs/concepts.md) — modes, hysteresis, mutual dependencies, Rollout support
- [Getting Started](docs/getting-started.md) — installation and first WorkloadDependency
- [API Reference](docs/api-reference.md) — complete CRD spec/status reference
- [Operations](docs/operations.md) — HA, monitoring, troubleshooting, upgrade guide

## Development

**Requirements:** Go 1.22+, kubebuilder v4, podman or docker

```bash
# Generate CRDs and deepcopy
make manifests generate

# Run tests (unit + integration)
make test

# Run e2e tests (requires cluster + klink deployed)
make test-e2e

# Build image
make image-build IMG=your-registry/klink:tag

# Push image
make image-push IMG=your-registry/klink:tag
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for full development guide.

## Roadmap

- Prometheus-based health conditions (`expr: rate(errors[2m]) < 0.01`)
- Dependency graph visualization and cycle detection
- DaemonSet support
