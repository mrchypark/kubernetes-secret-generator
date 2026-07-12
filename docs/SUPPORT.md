# v4.0.0-rc.9 support status

`v4.0.0-rc.9` is a release candidate. It has no SLA, capacity, high-availability, failover,
or production-certification claim.

| Component | Candidate target |
|---|---|
| Kubernetes | 1.34 and 1.35 API compatibility target; validate the intended cluster with preflight and smoke |
| Helm | 3.14+ operator target; repository checks use the exact version in `tools.lock` |
| Architecture | amd64 10–15 minute smoke; arm64 image build plus `--help` startup check |
| API | `secretgenerator.mittwald.de/v1alpha1` |
| Installation | Existing direct Helm or Flux lifecycle may be retained; do not mix CRD managers |

General bugs may use GitHub issues when they contain no sensitive values. Include candidate
digest, Kubernetes version, scope, redacted Conditions/Events, and reproduction steps.
Security reports follow [SECURITY.md](../SECURITY.md).

Broader guarantees are reconsidered after at least 10 independent clusters, two release
maintainers, an SLA/regulatory need, real arm64 production operation, or quarterly releases.
