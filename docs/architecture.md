# Architecture

## Overview

klink is a Kubernetes operator that manages dependency relationships between workloads. When a dependency becomes unhealthy, klink automatically suspends dependent workloads and restores them when the dependency recovers.

```mermaid
graph TB
    subgraph "Kubernetes Cluster"
        subgraph "klink-system"
            OP[klink operator]
            WH[gate webhook\nHTTPS :9443]
            TLS[TLS manager\nauto-rotate]
        end

        subgraph "User namespaces"
            WD[WorkloadDependency\nCRD]
            DEP[dependency\nDeployment/StatefulSet\nRollout]
            SVC[dependent\nDeployment/StatefulSet\nCronJob/Rollout]
        end

        API[Kubernetes\nAPI Server]
        ETCD[(etcd)]
    end

    OP -->|watches| WD
    OP -->|watches| DEP
    OP -->|scales/suspends| SVC
    WH -->|blocks scale-up\nwhen unhealthy| API
    TLS -->|patches caBundle| API
    API --- ETCD
```

## Reconciliation State Machine

Each `WorkloadDependency` transitions through these phases:

```mermaid
stateDiagram-v2
    [*] --> Healthy: created, all deps healthy

    Healthy --> Degraded: dependency becomes unhealthy\n(starts hysteresis window)
    Degraded --> Healthy: dependency recovers\nbefore window expires
    Degraded --> Suspended: window expires\n→ scale dependent to 0

    Suspended --> Suspended: dependency still unhealthy\n(strict: re-enforce every 15s)
    Suspended --> Healthy: dependency recovers\n+ recovery window passes\n→ restore replicas

    Healthy --> Paused: klink.dev/paused=true
    Degraded --> Paused: klink.dev/paused=true
    Suspended --> Paused: klink.dev/paused=true
    Paused --> Healthy: annotation removed\n(re-evaluates immediately)

    Healthy --> Unknown: dependent workload\nnot found
    Unknown --> Healthy: workload appears

    Healthy --> [*]: WD deleted\n(finalizer restores replicas first)
    Suspended --> [*]: WD deleted\n(finalizer restores replicas first)
```

## Scale-to-Zero Sequence

What happens when a dependency fails:

```mermaid
sequenceDiagram
    participant Dep as dependency<br/>(e.g. database)
    participant Op as klink operator
    participant WD as WorkloadDependency
    participant Svc as dependent<br/>(e.g. payments)

    Dep->>Dep: pods crash / readiness fails
    Op->>Dep: GET deployment status
    Op->>WD: status.phase = Degraded<br/>status.degradedSince = now
    Note over Op: waits for window (default 30s)

    Op->>Op: window expired
    Op->>Svc: PATCH spec.replicas = 0
    Op->>WD: status.phase = Suspended<br/>status.savedReplicas = 3

    Dep->>Dep: pods recover
    Op->>Dep: GET — readyReplicas/desiredReplicas ≥ 80%
    Op->>WD: status.healthySince = now
    Note over Op: waits for recoveryWindow (default 60s)

    Op->>Svc: PATCH spec.replicas = 3 (restored)
    Op->>WD: status.phase = Healthy<br/>status.savedReplicas = nil
```

## Gate Mode — Blocking Scale-Up

```mermaid
sequenceDiagram
    participant User as kubectl / HPA
    participant API as API Server
    participant WH as klink gate webhook
    participant Op as klink operator
    participant Dep as dependency

    User->>API: PATCH deployments/payments (replicas: 5)
    API->>WH: AdmissionReview{kind:Deployment, name:payments}
    WH->>Op: GET WorkloadDependency (mode=gate)
    WH->>Dep: GET — check health
    Dep-->>WH: ReadyReplicas=0, Desired=2

    WH-->>API: AdmissionResponse{allowed:false,\nmessage:"klink gate: dependency database unhealthy"}
    API-->>User: Error: admission webhook denied the request
```

## Mutual Dependency — CoSuspended Resolution

Prevents A→B + B→A deadlock:

```mermaid
sequenceDiagram
    participant Op as klink operator
    participant WDA as WD: bar-needs-foo
    participant WDB as WD: foo-needs-bar
    participant Foo as foo deployment
    participant Bar as bar deployment

    Note over Foo: foo crashes
    Op->>WDA: dep foo unhealthy → window starts
    Op->>WDA: window expired → scale bar to 0
    Note over WDA: phase=Suspended, savedReplicas=3

    Op->>WDB: dep bar has replicas=0
    Op->>WDB: isSuspendedByKlink? YES (WDA is Suspended)
    Note over WDB: bar = CoSuspended → treat as OK
    Note over WDB: foo stays at 0 (it's actually broken)

    Note over Foo: foo recovers
    Op->>WDA: foo healthy → start recoveryWindow
    Op->>WDA: window passed → restore bar to 3
    Note over WDA: phase=Healthy

    Op->>WDB: bar=3, foo healthy → Healthy
```

## TLS Certificate Lifecycle

```mermaid
sequenceDiagram
    participant Op as klink operator
    participant K8s as Kubernetes API
    participant WHC as ValidatingWebhook\nConfiguration

    Op->>K8s: GET Secret/klink-webhook-tls
    alt Secret not found or expiring soon
        Op->>Op: generate self-signed cert (1 year validity)
        Op->>K8s: CREATE/UPDATE Secret
    end
    Op->>WHC: PATCH caBundle = cert PEM
    Note over Op: every 12h: check cert expiry
    Note over Op: if < 30 days remaining → rotate
```
