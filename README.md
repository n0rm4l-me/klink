# klink

**Kubernetes operator for workload dependency management.**

klink automatically scales dependent services to zero when their dependencies become unhealthy — and restores them when dependencies recover. No more zombie services consuming resources and generating errors when their upstream is down.

[![Tests](https://github.com/n0rm4l-me/klink/actions/workflows/test.yml/badge.svg)](https://github.com/n0rm4l-me/klink/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22+-blue.svg)](go.mod)

---

## Why klink?

In microservice architectures, services often have hard runtime dependencies — a payments service that requires a database, a billing job that needs a message queue, a reporting service that depends on an analytics engine. When these dependencies go down, the dependent services keep running, burning CPU and memory, logging errors, and triggering false alerts.

klink solves this by treating workload dependencies as a first-class Kubernetes primitive.

**What klink does:**
- Automatically suspends dependent workloads when dependencies are unhealthy
- Restores them automatically when dependencies recover
- Prevents cascade failure scenarios with configurable hysteresis windows
- Handles mutual A↔B dependencies without deadlock
- Supports Deployments, StatefulSets, CronJobs, and Argo Rollouts (with canary-awareness)

---

## Supported workload types

| Kind | As dependent | As dependency | Notes |
|------|-------------|--------------|-------|
| `Deployment` | ✅ scale to 0 / restore | ✅ readyReplicas check | |
| `StatefulSet` | ✅ scale to 0 / restore | ✅ readyReplicas check | |
| `CronJob` | ✅ suspend / resume | — | No replicas concept |
| `Rollout` (Argo) | ✅ scale to 0 / restore | ✅ phase check | Suspension deferred during active canary |

---

## Use cases

### Cascade shutdown on dependency failure

When your database goes down, automatically suspend all services that depend on it — eliminating error storms, resource waste, and misleading alerts.

```yaml
apiVersion: deps.klink.dev/v1alpha1
kind: WorkloadDependency
metadata:
  name: payments-needs-database
  namespace: production
spec:
  dependent:
    kind: Deployment
    name: payments-service
  dependsOn:
    - kind: Deployment
      name: postgresql
      condition:
        minReadyPercent: 80   # tolerate one replica restart
        window: 30s            # ignore transient failures
        recoveryWindow: 60s    # wait for stability before restoring
  onDegraded:
    action: ScaleToZero
  mode: strict
```

### CronJob gating

Prevent batch jobs from running when their dependencies are unavailable — no more failed jobs cluttering your history.

```yaml
spec:
  dependent:
    kind: CronJob
    name: nightly-billing-export
  dependsOn:
    - kind: Deployment
      name: billing-service
  mode: soft
```

When `billing-service` is down, `nightly-billing-export` is automatically suspended (`spec.suspend: true`) and resumed when billing-service recovers.

### Argo Rollout canary awareness

klink is canary-aware. If a dependent Rollout is in the middle of a canary deployment when its dependency goes down, klink **defers** the scale-to-zero until the rollout completes — never interrupting an active deployment.

```yaml
spec:
  dependent:
    kind: Rollout
    name: payments-service
  dependsOn:
    - kind: Rollout
      name: auth-service
```

During canary (`status.phase: Progressing`), the stable version keeps serving traffic and klink waits.

### Mutual dependencies without deadlock

Services that depend on each other are handled gracefully. When klink suspends service-A because service-B failed, it marks service-A as `CoSuspended`. Other WDs that depend on service-A see it as intentionally suspended — not a real failure — and don't cascade further.

```
service-a ←→ service-b (mutual dependency)

service-b fails:
  → service-a suspended (CoSuspended)
  → b-needs-a: sees a=CoSuspended, stays Healthy (no deadlock)

You restore service-b manually:
  → service-a auto-restores ✓
```

### Gate mode — block scale-up until dependencies are healthy

Prevent operators or HPAs from scaling up a service while its dependencies are down.

```yaml
spec:
  mode: gate  # admission webhook blocks scale-up
```

On supported clusters (not GKE Autopilot), the admission webhook returns HTTP 403 with a clear message:

```
Error: admission webhook denied the request:
  klink gate: scale blocked by WorkloadDependency/payments-needs-database
  — dependency postgresql is not healthy: 0/3 ready (0% < 80%)
```

### Emergency override

Pause enforcement without deleting the resource — useful during incident response when you need manual control.

```bash
kubectl annotate workloaddependency payments-needs-database klink.dev/paused=true
# klink stops all enforcement immediately

kubectl annotate workloaddependency payments-needs-database klink.dev/paused-
# enforcement resumes
```

---

## How it works

```
dependency health:  ████░░░░░░░░░░░░░████████████████████
                         ↑                  ↑
                    starts failing      recovers

klink:               wait 30s→ scale to 0   wait 60s→ restore
                    [Degraded]  [Suspended]  [Suspended]  [Healthy]
```

1. klink watches for dependency health changes
2. When a dependency becomes unhealthy, starts the hysteresis `window` timer
3. If still unhealthy after `window` — scales dependent to 0, saves replica count
4. When dependency recovers, starts the `recoveryWindow` timer
5. After `recoveryWindow` — restores exact replica count

---

## Enforcement modes

| Mode | On dependency failure | On manual scale-up (while suspended) |
|------|----------------------|--------------------------------------|
| `strict` | Scale to 0 after window | Reverts to 0 within 15s |
| `soft` | Scale to 0 once | Respects manual override |
| `gate` | No automatic scale | Blocks scale-up via admission webhook |
| `observe` | **Logs only — no action** | N/A — use for onboarding/dry-run |

### Observe mode — safe onboarding

Apply klink to existing services without any risk. Observe mode logs what klink *would* do, but never touches your workloads:

```yaml
mode: observe
```

The WD phase becomes `Observed` when klink would have acted. Switch to `strict` or `soft` when you're confident.

### Safety net — maxSuspendDuration

Automatically restore workloads after a maximum suspension time, even if the dependency is still unhealthy:

```yaml
onDegraded:
  action: ScaleToZero
  maxSuspendDuration: 4h  # restore after 4 hours no matter what
```

Prevents services from being stuck at zero indefinitely due to long-running outages.

### Webhook notifications

Get notified when workloads are suspended or restored:

```yaml
notify:
  webhook: https://hooks.slack.com/services/xxx/yyy/zzz
  onPhases: [Suspended, Healthy]  # optional, defaults to these two
```

For secrets:
```yaml
notify:
  webhookSecretRef:
    name: slack-webhook
    key: url   # Secret key containing the URL
```

The notification payload:
```json
{
  "workloadDependency": "payments-needs-database",
  "namespace": "production",
  "phase": "Suspended",
  "previousPhase": "Degraded",
  "dependent": "payments-service",
  "dependentKind": "Deployment",
  "message": "dependency postgresql not healthy",
  "timestamp": "2026-06-12T10:00:00Z"
}
```

---

## Status phases

```
kubectl get workloaddependencies -A

NAMESPACE    NAME                       PHASE       REPLICAS   MESSAGE                               AGE
production   payments-needs-database    Suspended   3          dependency postgresql not healthy      5m
production   billing-needs-database     Suspended              CronJob suspended                     5m
staging      auth-needs-vault           Healthy                all dependencies healthy              2d
```

| Phase | Meaning |
|-------|---------|
| `Healthy` | All dependencies healthy, workload running normally |
| `Degraded` | Dependency unhealthy, within hysteresis window — no action yet |
| `Suspended` | Dependent scaled to 0 (or CronJob suspended) |
| `Paused` | `klink.dev/paused=true` annotation set |
| `Unknown` | Dependent workload not found |

---

## Installation

```bash
helm upgrade --install klink oci://ghcr.io/n0rm4l-me/charts/klink \
  --namespace klink-system \
  --create-namespace
```

Or from source:

```bash
git clone https://github.com/n0rm4l-me/klink
helm upgrade --install klink ./charts/klink \
  --namespace klink-system \
  --create-namespace \
  --set image.tag=0.2.0
```

### Enable gate mode (optional, requires admission webhook support)

```bash
helm upgrade klink ./charts/klink \
  --namespace klink-system \
  --set gateWebhook.enabled=true
```

> **Note:** Gate mode via admission webhook is not supported on GKE Autopilot clusters (platform limitation). Use `strict` mode instead — it achieves the same result by reverting unauthorized scale-ups within 15 seconds.

---

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
        window: 30s            # default 30s — ignore transient failures
        recoveryWindow: 60s    # default 60s — wait for stability

  onDegraded:
    action: ScaleToZero

  mode: strict | soft | gate   # default strict
```

---

## Metrics

klink exposes Prometheus metrics at `:8080/metrics` (enabled by default).

| Metric | Type | Description |
|--------|------|-------------|
| `klink_dependency_phase{namespace,name,phase}` | Gauge | Current phase (1=active) |
| `klink_scale_to_zero_total{namespace,kind,name}` | Counter | Scale-to-zero actions |
| `klink_replicas_restored_total{namespace,kind,name}` | Counter | Replica restore actions |
| `klink_reconcile_errors_total{namespace,error_type}` | Counter | Reconciliation errors |
| `klink_suspended_workloads{namespace,kind}` | Gauge | Currently suspended count |

GKE Managed Prometheus: `PodMonitoring` resource created automatically when `metrics.enabled=true`.

---

## k9s plugins

Copy `contrib/k9s-plugins.yaml` into `~/.config/k9s/plugins.yaml` and navigate to `:workloaddependencies` in k9s.

| Shortcut | Action |
|----------|--------|
| `d` | Describe WorkloadDependency |
| `Ctrl-E` | Show events |
| `Ctrl-L` | Operator logs |
| `Ctrl-S` | Show dependent workload |
| `Ctrl-F` | Force reconcile |

---

## Documentation

| Document | Description |
|---------|-------------|
| [Architecture](docs/architecture.md) | Component diagrams, state machine, sequence diagrams |
| [Core Concepts](docs/concepts.md) | Modes, hysteresis, mutual deps, Rollout canary behavior |
| [Getting Started](docs/getting-started.md) | Installation and first WorkloadDependency |
| [API Reference](docs/api-reference.md) | Complete CRD spec/status reference |
| [Operations](docs/operations.md) | HA, monitoring, troubleshooting, upgrade guide |

---

## Development

```bash
# Run tests (unit + integration)
make test

# Run e2e tests against a real cluster
make test-e2e

# Build image
make image-build IMG=your-registry/klink:tag
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full development guide.

---

## Roadmap

- Prometheus-based health conditions (`expr: rate(errors[2m]) < 0.01`)
- `kubectl klink` plugin — graph visualization, status, why-suspended
- DaemonSet support
