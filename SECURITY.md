# Security Policy

## Supported versions

| Version | Security fixes |
|---|---|
| `v4.0.0-rc.10` | Release candidate; security reports accepted, no production support claim |
| v3 and older | Best effort while evaluating the v4 release candidate |

Supported Kubernetes, deployment tools, and CPU architectures are listed in [docs/SUPPORT.md](docs/SUPPORT.md).

## Report a vulnerability

Do not open a public issue. Email `opensource@mittwald.de` with the affected version, impact, reproduction steps, and a safe contact method. Do not include live Secret values, private keys, tokens, cluster credentials, or unredacted backups.

We acknowledge reports within 2 business days, provide an initial severity assessment within 5 business days, and target a remediation plan within 10 business days. Resolution time depends on severity and coordinated disclosure requirements. Reporters receive status at least every 10 business days until closure.

## Operator security requirements

- Deploy a verified image digest; do not deploy mutable tags such as `latest`.
- Verify the candidate image signature, SBOM, and vulnerability result as described in [docs/RELEASE.md](docs/RELEASE.md).
- Use least-privilege scope/RBAC, Kubernetes Secret at-rest encryption, restricted Pod Security, and encrypted access-controlled backups.
- Treat CR `spec.data`, `spec.privateKey`, generated Secrets, tracking annotations, and backup payloads as sensitive.
- Never expose Secret data or managed checksums in logs, metrics, CI artifacts, support cases, or incident reports.

## Release exceptions

Critical and High image findings block release. A temporary waiver must identify one vulnerability and include an owner, rationale, compensating controls, and an expiry within 90 days. The protected branch ruleset must require the CODEOWNERS review declared for `.github/vulnerability-waivers.json`; merging a structurally valid file without that security approval is not an approved waiver. Expired, duplicate, incomplete, or overlong waivers fail the release workflow.
