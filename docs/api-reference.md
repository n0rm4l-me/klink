# API Reference

## WorkloadDependency

**Group:** `deps.klink.dev`  
**Version:** `v1alpha1`  
**Kind:** `WorkloadDependency`  
**Scope:** Namespaced

### Spec

```yaml
spec:
  dependent:
    kind: string          # Deployment | StatefulSet | CronJob | Rollout
    name: string          # required
    namespace: string     # optional; defaults to WorkloadDependency namespace

  dependsOn:
    - kind: string        # Deployment | StatefulSet | Rollout (not CronJob)
      name: string        # required
      namespace: string   # optional; cross-namespace supported
      condition:
        minReadyPercent: integer  # 0-100, default 100
        window: duration          # default "30s"
        recoveryWindow: duration  # default "60s"

  onDegraded:
    action: ScaleToZero           # only supported action
    maxSuspendDuration: duration  # optional; auto-restore after this time (e.g. "4h")

  mode: strict | soft | gate | observe  # default "strict"

  notify:                         # optional; send webhook on phase transitions
    webhook: string               # URL to POST to (mutually exclusive with webhookSecretRef)
    webhookSecretRef:
      name: string                # Secret name
      key: string                 # Secret key containing URL (default: "url")
    onPhases: []                  # phases that trigger notification (default: [Suspended, Healthy])
```

### Spec Fields

#### `spec.dependent`

The workload that will be suspended when dependencies become unhealthy.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `kind` | string | yes | `Deployment`, `StatefulSet`, `CronJob`, or `Rollout` |
| `name` | string | yes | Name of the workload |
| `namespace` | string | no | Defaults to `WorkloadDependency.metadata.namespace` |

**CronJob**: sets `spec.suspend=true` instead of scaling to 0. `savedReplicas` is not used.

**Rollout**: if a canary or blue-green rollout is `Progressing`, suspension is deferred until the rollout completes.

#### `spec.dependsOn[]`

List of workloads to monitor. All must be healthy for the dependent to remain running.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `kind` | string | yes | `Deployment`, `StatefulSet`, or `Rollout` |
| `name` | string | yes | Name of the workload |
| `namespace` | string | no | Defaults to `WorkloadDependency.metadata.namespace` |
| `condition.minReadyPercent` | integer | no | Minimum `readyReplicas/desiredReplicas × 100`. Default: `100` |
| `condition.window` | duration | no | Time dependency must be unhealthy before acting. Default: `30s` |
| `condition.recoveryWindow` | duration | no | Time dependency must be healthy before restoring. Default: `60s` |

For `Rollout` as dependency: `minReadyPercent` is not used. Health is determined by `status.phase`:
- `Healthy`, `Progressing`, `Paused` → healthy  
- `Degraded` → unhealthy

#### `spec.mode`

| Value | Behavior |
|-------|----------|
| `strict` | Scales to 0 on failure. Re-enforces scale-to-zero every 15s while unhealthy. |
| `soft` | Scales to 0 once. Does not revert manual scale-ups. |
| `gate` | Does not scale to 0. Blocks scale-up via admission webhook while unhealthy. |
| `observe` | Logs what klink would do but takes **no action**. Use for safe onboarding. Phase becomes `Observed`. |

#### `spec.onDegraded`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `action` | string | `ScaleToZero` | Action to take. Currently only `ScaleToZero`. |
| `maxSuspendDuration` | duration | `""` (disabled) | Auto-restore workload after this time even if dependency still unhealthy. Prevents indefinite suspension. E.g. `"4h"`. After restore phase becomes `Released`. |

#### `spec.notify`

Optional. Sends an HTTP POST to a webhook URL on phase transitions.

| Field | Type | Description |
|-------|------|-------------|
| `webhook` | string | Webhook URL. Mutually exclusive with `webhookSecretRef`. |
| `webhookSecretRef.name` | string | Secret containing the URL. |
| `webhookSecretRef.key` | string | Secret key. Default: `"url"`. |
| `onPhases` | string[] | Phases that trigger notification. Default: `["Suspended", "Healthy"]`. |

Payload sent on each notification:
```json
{
  "workloadDependency": "payments-needs-database",
  "namespace": "production",
  "phase": "Suspended",
  "previousPhase": "Degraded",
  "dependent": "payments-service",
  "dependentKind": "Rollout",
  "message": "dependency postgresql not healthy",
  "timestamp": "2026-06-12T10:00:00Z"
}
```

### Status

```yaml
status:
  phase: Healthy | Degraded | Suspended | Released | Observed | Paused | Unknown
  savedReplicas: integer   # replica count saved before scale-to-zero
  degradedSince: time      # when dependency first became unhealthy
  suspendedAt: time        # when workload was suspended (used for maxSuspendDuration)
  healthySince: time       # when dependency first recovered (for recoveryWindow)
  message: string          # human-readable description
  conditions: []           # standard metav1.Condition array
```

#### `status.phase`

| Phase | Meaning |
|-------|---------|
| `Healthy` | All dependencies healthy; dependent running normally |
| `Degraded` | Dependency unhealthy; within hysteresis window, not yet acted |
| `Suspended` | Dependent scaled to 0 (or CronJob suspended) |
| `Released` | Force-restored after `maxSuspendDuration` expired while dependency was still unhealthy; klink will not re-suspend until the dependency genuinely recovers |
| `Observed` | Observe mode — klink would have acted but took no action |
| `Paused` | `klink.dev/paused=true` annotation set; no enforcement |
| `Unknown` | Dependent workload not found |

### Annotations

| Annotation | Value | Effect |
|-----------|-------|--------|
| `klink.dev/paused` | `"true"` | Suspend all enforcement; phase becomes `Paused` |

### Events

klink emits the following Kubernetes events on the `WorkloadDependency` object:

| Reason | Type | Trigger |
|--------|------|---------|
| `ScaledToZero` | Warning | Dependent scaled to 0 replicas |
| `ReplicasRestored` | Normal | Dependent replicas restored after recovery |
| `CronJobSuspended` | Warning | CronJob `spec.suspend` set to `true` |
| `CronJobResumed` | Normal | CronJob `spec.suspend` set to `false` |
| `StrictEnforced` | Warning | Manual scale-up reverted (strict mode) |

### Finalizer

klink adds `klink.dev/finalizer` to every `WorkloadDependency`. On deletion, if the dependent is currently `Suspended`, klink restores its replicas before removing the finalizer.

### Printer Columns

```
NAME    PHASE     REPLICAS   MESSAGE   AGE
```

- **PHASE**: current `status.phase`
- **REPLICAS**: `status.savedReplicas` — replica count saved before suspension
- **MESSAGE**: `status.message` — human-readable state description
