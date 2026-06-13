# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added
- `--watch-namespace` flag for single-namespace mode (multi-tenancy)
- `watchNamespace` value in Helm chart
- `ValidatingWebhookConfiguration/klink-wd-validator` — validates WorkloadDependency on CREATE/UPDATE (failurePolicy: Fail)
- WD validation: CronJob as dependency rejected, negative durations rejected, notify config validated
- Release workflow (`.github/workflows/release.yml`): multi-arch image (amd64+arm64), GHCR push, Helm OCI chart, cosign signing, SBOM, GitHub Release
- Notification webhook retry with exponential backoff (3 attempts: 1s, 2s, 4s)
- `Released` phase — after `maxSuspendDuration` klink stays hands-off until dependency recovers

### Fixed
- `maxSuspendDuration` flapping loop — repeated suspend/restore cycle broken by `Released` phase
- Status update conflict handled by requeue instead of error
- `imagePullPolicy` defaults to `IfNotPresent` (was `Always`, blocked airgapped deployments)

## [0.2.0] - 2026-06-12

### Added
- Prometheus metrics: `klink_dependency_phase`, `klink_scale_to_zero_total`, `klink_replicas_restored_total`, `klink_reconcile_errors_total`, `klink_suspended_workloads`
- GKE PodMonitoring resource in Helm chart for GCP Managed Prometheus
- `status.conditions[]` populated on every phase transition (standard k8s condition management)
- e2e test suite in `test/e2e/` (5 tests: lifecycle, mutual deps, finalizer, pause, strict mode)
- Webhook unit tests (10 tests, no cluster required)
- Field index for `isSuspendedByKlink` — O(1) lookup replaces cluster-wide O(n) List()
- Finalizer `klink.dev/finalizer` — restores replicas when WorkloadDependency is deleted while suspended
- Panic recovery in admission webhook ServeHTTP
- `readOnlyRootFilesystem: true` + explicit `runAsUser: 65532` in pod security context
- PodDisruptionBudget when `replicaCount > 1`
- `NOTES.txt` in Helm chart with post-install instructions
- Namespaced Role for TLS secret (was cluster-wide ClusterRole)
- `terminationGracePeriodSeconds: 30` (was 10)
- docs/ directory with mermaid diagrams: architecture, concepts, getting-started, api-reference, operations
- SECURITY.md, CHANGELOG.md, CONTRIBUTING.md, CODE_OF_CONDUCT.md, MAINTAINERS

### Fixed
- Pod naming: `klink-klink` → `klink` (Helm fullname deduplication)
- Nil panic on `wd.Annotations` map when annotations not set
- Webhook graceful shutdown with 10s timeout (was context.Background())
- Gate mode no longer scales dependent to zero (only tracks health for webhook)
- caBundle patched on every operator startup, not only on cert generation/rotation
- `io.ReadAll` for webhook body (was `r.ContentLength` which can be -1)

## [0.1.0] - 2026-06-10

### Added
- `WorkloadDependency` CRD for declaring workload dependency relationships
- Scale-to-zero when dependency becomes unhealthy (Deployment, StatefulSet, Rollout)
- CronJob suspend/resume support
- Argo Rollout support with defer-during-canary logic
- Hysteresis window — configurable delay before acting on unhealthy dependency
- Recovery window — configurable stabilization period before restoring replicas
- `strict` mode — re-enforces scale-to-zero every 15s; reverts manual overrides
- `soft` mode — scales to zero once, respects manual overrides
- `gate` mode — admission webhook blocks scale-up while dependency is unhealthy
- CoSuspended detection — prevents deadlock in mutual A→B + B→A dependencies
- `klink.dev/paused` annotation — temporarily suspend enforcement
- Finalizer — restores replicas when WorkloadDependency is deleted while suspended
- Self-managed TLS for admission webhook — auto-rotates 30 days before expiry
- Helm chart with optional gate webhook (`gateWebhook.enabled`)
- k9s plugins in `contrib/k9s-plugins.yaml`
- Cross-namespace dependency support
- 18 integration tests via envtest
- GitHub Actions CI

[Unreleased]: https://github.com/n0rm4l-me/klink/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/n0rm4l-me/klink/releases/tag/v0.1.0
