# klink

Kubernetes operator for workload dependency management. Automatically scales dependent services to zero when their dependencies become unhealthy, and restores them when dependencies recover.

## How it works

You declare a `WorkloadDependency` resource that links two deployments:

```yaml
apiVersion: deps.klink.dev/v1alpha1
kind: WorkloadDependency
metadata:
  name: payments-needs-database
  namespace: my-app
spec:
  dependent:
    kind: Deployment
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

## Enforcement modes

| Mode | Behavior |
|------|----------|
| `strict` | Re-enforces scale-to-zero if someone manually scales up while dependency is down. Reverts within 15s. |
| `soft` | Scales to zero once but does not fight manual changes. |
| `gate` | Blocks dependent from starting until dependency is healthy (v0.2, requires admission webhook). |

## Pausing

To temporarily disable enforcement without deleting the resource:

```bash
kubectl annotate workloaddependency payments-needs-database klink.dev/paused=true

# Resume:
kubectl annotate workloaddependency payments-needs-database klink.dev/paused-
```

Phase becomes `Paused` while annotation is set.

## Mutual dependencies

klink handles the A→B + B→A case without deadlock. When `payments` is scaled to zero by klink, it is marked as `CoSuspended`. Other `WorkloadDependency` objects that depend on `payments` treat it as a non-failure — they won't cascade-suspend their own dependents.

When you manually restore one service, klink automatically restores the other.

## Status

```
kubectl get workloaddependencies -A

NAMESPACE  NAME                 PHASE       REPLICAS   MESSAGE                              AGE
my-app     payments-needs-database   Suspended   2          dependency database not healthy (0/0)   5m
```

| Phase | Meaning |
|-------|---------|
| `Healthy` | All dependencies healthy |
| `Degraded` | Dependency unhealthy, within hysteresis window |
| `Suspended` | Dependent scaled to zero |
| `Paused` | Enforcement disabled via annotation |
| `Unknown` | Dependent deployment not found |

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
    kind: Deployment      # only Deployment supported in v0.1
    name: string
    namespace: string     # optional, defaults to WorkloadDependency namespace

  dependsOn:
    - kind: Deployment
      name: string
      namespace: string   # optional, cross-namespace supported
      condition:
        minReadyPercent: 100   # 0-100, default 100
        window: 30s            # default 30s
        recoveryWindow: 60s    # default 60s

  onDegraded:
    action: ScaleToZero   # only action in v0.1

  mode: strict            # strict | soft, default strict
```

## Development

**Requirements:** Go 1.22+, kubebuilder v4, podman or docker

```bash
# Generate CRDs and deepcopy
make manifests generate

# Run tests (downloads envtest binaries automatically)
make test

# Build image
make image-build

# Push image
make image-push
```

## Roadmap (v0.2)

- StatefulSet and DaemonSet support
- `gate` mode via admission webhook
- CronJob suspend when dependency unavailable
- Prometheus-based health conditions
- Dependency graph visualization
- Cycle detection
