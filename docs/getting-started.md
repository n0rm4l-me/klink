# Getting Started

## Prerequisites

- Kubernetes 1.25+
- Helm 3.x
- `kubectl` configured

For **gate mode**: a cluster that supports ValidatingAdmissionWebhooks (any standard cluster; not GKE Autopilot).

For **Argo Rollout** support: Argo Rollouts installed in the cluster.

## Installation

```bash
helm upgrade --install klink oci://ghcr.io/n0rm4l-me/charts/klink \
  --namespace klink-system \
  --create-namespace
```

Or from source:

```bash
git clone https://github.com/n0rm4l-me/klink
cd klink
helm upgrade --install klink ./charts/klink \
  --namespace klink-system \
  --create-namespace \
  --set image.tag=0.2.0
```

Verify the operator is running:

```bash
kubectl get pods -n klink-system
# NAME                          READY   STATUS    RESTARTS   AGE
# klink-klink-6db74b8fb-wpdhq   1/1     Running   0          30s
```

## Your First WorkloadDependency

Suppose you have a `payments` Deployment that requires a `database` Deployment to be healthy:

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
        minReadyPercent: 80   # consider healthy if ≥80% pods ready
        window: 30s            # wait 30s before acting
        recoveryWindow: 60s    # wait 60s after recovery before restoring

  onDegraded:
    action: ScaleToZero

  mode: strict
```

Apply it:

```bash
kubectl apply -f workloaddependency.yaml
```

## Verify It Works

Check the status:

```bash
kubectl get workloaddependencies -n my-app

# NAME                       PHASE     REPLICAS   MESSAGE                    AGE
# payments-needs-database    Healthy              all dependencies healthy   10s
```

Simulate a failure by scaling database to 0:

```bash
kubectl scale deployment database -n my-app --replicas=0
```

Watch what happens:

```bash
# After ~30s (window):
kubectl get workloaddependencies -n my-app
# NAME                       PHASE       REPLICAS   MESSAGE                              AGE
# payments-needs-database    Suspended   3          dependency database not healthy       45s

kubectl get deployment payments -n my-app
# NAME       READY   UP-TO-DATE   AVAILABLE   AGE
# payments   0/0     0            0           5m
```

Restore the database:

```bash
kubectl scale deployment database -n my-app --replicas=2
```

After ~60s (recoveryWindow), payments is automatically restored:

```bash
kubectl get deployment payments -n my-app
# NAME       READY   UP-TO-DATE   AVAILABLE   AGE
# payments   3/3     3            3           6m
```

## Common Operations

### Check status of all WorkloadDependencies

```bash
kubectl get workloaddependencies -A
```

### Pause enforcement (manual override)

```bash
kubectl annotate workloaddependency payments-needs-database \
  -n my-app klink.dev/paused=true

# Resume:
kubectl annotate workloaddependency payments-needs-database \
  -n my-app klink.dev/paused-
```

### Force immediate reconciliation

```bash
kubectl annotate workloaddependency payments-needs-database \
  -n my-app klink.dev/reconcile="$(date +%s)" --overwrite
```

### View events

```bash
kubectl get events -n my-app \
  --field-selector involvedObject.name=payments-needs-database \
  --sort-by=.lastTimestamp
```

### View operator logs

```bash
kubectl logs -n klink-system -l app.kubernetes.io/name=klink -f
```

### Use k9s plugins

Install the k9s plugins from `contrib/k9s-plugins.yaml` and navigate to `:workloaddependencies` in k9s.

Available shortcuts:
- `d` — describe
- `Ctrl-E` — show events
- `Ctrl-L` — operator logs
- `Ctrl-S` — show dependent workload
- `Ctrl-F` — force reconcile
