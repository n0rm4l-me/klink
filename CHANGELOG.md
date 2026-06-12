# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

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
