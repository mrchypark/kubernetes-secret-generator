# v4.0.0-rc.12 release process

`v4.0.0-rc.12` is a candidate, not an SLA or capacity claim. Promotion is a manual,
maintainer-owned decision recorded in a GitHub issue and protected by the repository's
GitHub Environment.

## Candidate build

1. Prepare the annotated `v4.0.0-rc.12` tag from the intended source commit, without pushing
   it yet. Confirm `Chart.yaml` version/appVersion and `values.yaml` image tag match rc.12.
2. Open the GitHub release issue with owner, rollback contact, target cluster inventory,
   links to [migration](MIGRATION-v4.md), [backup/restore](BACKUP-RESTORE.md),
   [rollback](ROLLBACK.md), and [support status](SUPPORT.md), plus a checklist item for a
   redacted zero-blocker preflight report.
3. Confirm the source, unit, race, and envtest checks have passed in PR or `master` CI, then
   push the tag. The candidate workflow validates release inputs, builds and starts the image,
   scans it, generates an SBOM, signs artifacts, packages the chart, and runs the kind smoke.
4. Build `linux/amd64` and `linux/arm64` images and record the manifest-list digest, chart
   digest, source commit, workflow run link, and candidate artifact links in the issue.
5. Do not use `latest`, rebuild the candidate during promotion, or place Secret values,
   managed checksums, kubeconfigs, or plaintext backups in the issue or artifacts.

## Migration evidence

Run the public read-only preflight against the explicitly approved target. JSON is useful
for automation; Markdown is suitable for the issue.

```sh
export KUBE_CONTEXT=approved-cluster
export CONFIRM_CONTEXT="$KUBE_CONTEXT"
export EXPECTED_SERVER_URL=https://approved-api.example.invalid:6443
export EXPECTED_CA_SHA256='<approved-64-hex-ca-fingerprint>'
export NAMESPACE=secret-generator-system
export RELEASE_NAME=kubernetes-secret-generator
export SCOPE_MODE=ownNamespace
export CONFIRMED_SCOPE=ownNamespace
REPORT_FORMAT=markdown REPORT_FILE="$PWD/preflight.md" scripts/preflight-v4.sh
```

The command performs no apply/create/delete/patch/replace operation. Resolve all blockers;
review warnings for stale keys or labels and record the disposition without copying data.

## Candidate validation

- On amd64, run the complete disposable-cluster smoke for at least 10 and at most 15
  minutes. It checks Ready state, restart/fatal errors, owner conflicts, unexpected
  rotations, and basic create/reconcile behavior throughout its candidate phases.
- Build the arm64 image and run its startup path, for example
  `docker run --rm --platform linux/arm64 IMAGE@DIGEST --help`. This is a build/startup
  check, not real-arm64 production certification.
- The existing 100-object benchmark may be run as optional diagnostic information. Report
  the environment and raw summary, but do not call it capacity, an SLO, or an SLA.
- Complete the manual basic backup/restore rehearsal and attach only its redacted result.

## Manual promotion

Attach redacted outputs and exact digests to the GitHub issue. Copy the candidate workflow
outputs directly into the promotion dispatch inputs: candidate run ID, image digest,
source commit, and chart digest. A maintainer verifies those values against the issue,
confirms rollback readiness and unresolved risks, then closes the completed checklist.
Manually dispatch promotion with that closed issue and approve the protected GitHub
Environment. Promotion must reuse the candidate digest. After publication, record the
result in the closed issue; on failure, reopen it with the failure and rollback decision.

Heavier availability, long-running resilience, multi-environment matrix, HA, and PDB gates
are deferred. Reintroduction thresholds are in [the development
directive](PRODUCTION_READINESS_DEVELOPMENT_DIRECTIVE.md).
