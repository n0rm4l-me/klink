# Operations Guide

## High Availability

By default klink runs with 1 replica and leader election enabled. For HA:

```yaml
# values.yaml
replicaCount: 2
leaderElection:
  enabled: true  # required when replicaCount > 1
```

With 2+ replicas, a PodDisruptionBudget is automatically created (minAvailable: 1).

## Monitoring

### Key metrics to watch

klink exposes standard controller-runtime metrics at `:8080/metrics` (disabled by default, enable via `--metrics-bind-address=:8080`).

Recommended alerts:

| Alert | Query pattern | Severity |
|-------|--------------|---------|
| Operator down | `up{job="klink"} == 0` for 5m | Critical |
| High reconcile errors | `rate(controller_runtime_reconcile_errors_total[5m]) > 0.1` | Warning |
| Workload stuck suspended | `kube_customresource_status_phase{kind="WorkloadDependency", phase="Suspended"} > 0` for 2h | Warning |

### Grafana dashboard

Key panels to build:
- WorkloadDependency phases by namespace (pie chart)
- Reconcile duration histogram
- Scale-to-zero events over time
- Recovery time distribution

## Troubleshooting

### WorkloadDependency stays Degraded but never Suspends

Check that `window` hasn't been set very long:

```bash
kubectl get workloaddependency my-wd -o jsonpath='{.spec.dependsOn[0].condition.window}'
```

Also check operator logs:

```bash
kubectl logs -n klink-system -l app.kubernetes.io/name=klink | grep "within window"
```

### Dependent stays at 0 after dependency recovered

Check `recoveryWindow` and `healthySince`:

```bash
kubectl get workloaddependency my-wd -o yaml | grep -A5 status
```

If `healthySince` is null, the dependency may still be unstable. Check:

```bash
kubectl get workloaddependency my-wd -o jsonpath='{.status.message}'
```

### Gate webhook blocking unexpectedly

On GKE Autopilot, the admission webhook for `apps/deployments` is not invoked (platform limitation). Use `strict` mode instead.

On other clusters, check if the webhook is reachable:

```bash
kubectl get validatingwebhookconfiguration klink-gate -o yaml | grep caBundle
```

If `caBundle` is empty, restart the operator to trigger re-patching.

### Operator logs show "forbidden" errors

Regenerate RBAC:

```bash
helm upgrade klink ./charts/klink --namespace klink-system --reuse-values
```

### WD deleted but workload stays at 0

This shouldn't happen with the finalizer in place. If it does:

```bash
# Manually restore
kubectl scale deployment my-svc --replicas=3
```

Check if finalizer is stuck:

```bash
kubectl get workloaddependency my-wd -o jsonpath='{.metadata.finalizers}'
# If operator is gone, manually remove:
kubectl patch workloaddependency my-wd --type=json \
  -p='[{"op":"remove","path":"/metadata/finalizers/0"}]'
```

## Upgrade Guide

### CRD upgrades

Always apply CRD updates before upgrading the operator:

```bash
kubectl apply -f https://raw.githubusercontent.com/n0rm4l-me/klink/main/testdata/crd/deps.klink.dev_workloaddependencies.yaml
helm upgrade klink ./charts/klink --namespace klink-system --reuse-values
```

### Breaking changes

See [CHANGELOG.md](../CHANGELOG.md) for breaking changes between versions.

## GKE Autopilot Note

GKE Autopilot does not invoke ValidatingAdmissionWebhooks for `apps/v1` Deployment resources. This means **gate mode** will not block scale-up operations via `kubectl scale` or HPA on Autopilot clusters.

**Workaround**: use `strict` mode instead. It achieves a similar result by reverting unauthorized scale-ups within 15 seconds of detection.

`gate` mode works correctly on:
- GKE Standard
- EKS
- AKS
- kind/minikube/k3d
- kubeadm clusters

## Resource Requirements

Default resource limits:

```yaml
resources:
  limits:
    cpu: 500m
    memory: 128Mi
  requests:
    cpu: 10m
    memory: 64Mi
```

For clusters with >100 WorkloadDependencies, increase memory to 256Mi and CPU to 100m.
