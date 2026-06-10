# klink

Kubernetes operator for workload dependency management. Automatically scales dependent services to zero when their dependencies become unhealthy, and restores them when dependencies recover.

## Supported workload types

| Kind | As dependent | As dependency | Notes |
|------|-------------|--------------|-------|
| `Deployment` | ✅ scale to 0 | ✅ readyReplicas check | |
| `StatefulSet` | ✅ scale to 0 | ✅ readyReplicas check | |
| `CronJob` | ✅ suspend=true | — | no replicas concept |
| `Rollout` (Argo) | ✅ scale to 0 | ✅ phase check | Progressing = healthy as dependency; suspension deferred during active canary |

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

  mode: strict  # strict | soft
```

When `database` becomes unhealthy:
1. klink waits for `window` (hysteresis — ignores transient restarts)
2. Scales `payments` to 0, saving its replica count
3. When `database` recovers, waits for `recoveryWindow`, then restores `payments`

For **CronJob** dependents: sets `spec.suspend=true` instead of scaling. Resumes on recovery.

For **Rollout** dependents: if a canary or blue-green rollout is in progress (`Progressing` phase), suspension is **deferred** until the rollout completes — klink never interrupts an active deployment.

## Enforcement modes

| Mode | Behavior |
|------|----------|
| `strict` | Re-enforces scale-to-zero on every reconcile while dependency is down. Manual scale-up reverted within 15s. |
| `soft` | Scales to zero once but does not fight manual changes. |
| `gate` | Blocks dependent from starting until dependency is healthy (v0.2, requires admission webhook). |

## Pausing

```bash
kubectl annotate workloaddependency payments-needs-database klink.dev/paused=true

# Resume:
kubectl annotate workloaddependency payments-needs-database klink.dev/paused-
```

Phase becomes `Paused` while annotation is set. klink stops all enforcement.

## Mutual dependencies

klink handles A→B + B→A without deadlock. When klink scales a service to zero, other `WorkloadDependency` objects that depend on it recognize it as `CoSuspended` (not a real failure) and don't cascade.

When you manually restore one service, klink automatically restores the other.

## Status

```
kubectl get workloaddependencies -A

NAMESPACE  NAME                      PHASE       REPLICAS   MESSAGE                                    AGE
my-app     payments-needs-database   Suspended   3          dependency database not healthy (0/0)      5m
my-app     billing-needs-database    Suspended              dependency database not healthy — CronJob   5m
```

| Phase | Meaning |
|-------|---------|
| `Healthy` | All dependencies healthy |
| `Degraded` | Dependency unhealthy, within hysteresis window |
| `Suspended` | Dependent scaled to zero (or CronJob suspended) |
| `Paused` | Enforcement disabled via `klink.dev/paused` annotation |
| `Unknown` | Dependent workload not found |

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
  --set image.tag=0.1.0
```

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

  mode: strict | soft     # default strict
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

## Development

**Requirements:** Go 1.22+, kubebuilder v4, podman or docker

```bash
# Generate CRDs and deepcopy
make manifests generate

# Run tests
make test

# Build image
make image-build IMG=your-registry/klink:tag

# Push image
make image-push IMG=your-registry/klink:tag
```

## Roadmap

- `gate` mode — admission webhook blocks dependent from starting until dependency is healthy
- Prometheus-based health conditions
- Dependency graph visualization and cycle detection
- DaemonSet support
