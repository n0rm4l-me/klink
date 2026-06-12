# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| 0.1.x   | ✅ |

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Report security vulnerabilities via [GitHub Security Advisories](https://github.com/n0rm4l-me/klink/security/advisories/new).

Please include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

**Response timeline:**
- Acknowledgement within 48 hours
- Status update within 7 days
- Fix target: 90 days for critical, 6 months for others

## Security Considerations for Users

### RBAC
klink requires ClusterRole permissions to watch and patch workloads across namespaces. Review `charts/klink/templates/rbac.yaml` before deploying.

### Gate Webhook TLS
When `gateWebhook.enabled=true`, klink generates a self-signed TLS certificate stored in `Secret/klink-webhook-tls`. The certificate rotates automatically 30 days before expiry.

### Pod Security
The operator runs as non-root (UID 65532) with `readOnlyRootFilesystem=true` and all capabilities dropped.
