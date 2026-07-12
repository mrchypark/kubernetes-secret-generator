# v4.0.0-rc.7 production-readiness directive

## Goal

Ship a reviewable `v4.0.0-rc.7` candidate with safe migration tooling and clear rollback.
This release does not claim an SLA, capacity, high availability, automatic failover, or
production certification. Existing security and API correctness checks remain mandatory.

## Required scope

1. Keep controller API validation, exact ownership, type immutability, deterministic
   regeneration, Secret self-heal behavior, status/Condition reporting, and redaction.
2. Provide one public `scripts/preflight-v4.sh` command. It is read-only, requires an
   explicit context/server/CA/namespace/release, validates a stable snapshot and crypto
   baseline in memory, and emits redacted JSON or Markdown.
3. Inventory owner/type/immutable/invalid/regeneration/stale/scope findings. A blocker is
   corrected before upgrade; warnings receive human review in the release issue.
4. Upgrade CRDs before the manager. Do not downgrade CRDs during manager rollback.
5. Preserve encrypted backup/restore and manager rollback runbooks.
6. Build the candidate in GitHub Actions, attach digests and test summaries to a GitHub
   release issue, and use the protected GitHub Environment for the manual promotion step.
7. Run an amd64 smoke test for at least 10 and at most 15 minutes, and build the arm64
   image and verify its startup path with `--help`. A disposable 100-object
   benchmark is optional diagnostic data and must not be described as capacity or an SLA.

## Deferred scope

The following are deliberately not per-release blockers for `v4.0.0-rc.7`: automated
multi-stage attestations, long-running resilience qualification, architecture/minor upgrade
matrices, failover certification, mandatory dual deployment-path rehearsal, HA topology,
and PDB availability claims.

Reintroduce these controls only when at least one of these thresholds is met:

- the controller is operated in at least 10 independent clusters;
- at least two maintainers can own release operations;
- an SLA or regulatory requirement exists;
- real arm64 production operation needs upgrade certification; or
- releases occur at least quarterly and justify repeatable release automation.

## Release checklist

- [ ] source/unit/race/envtest, generated artifacts, chart schema/render, and security scans pass;
- [ ] public preflight JSON or Markdown has zero blockers and contains no Secret material;
- [ ] backup is encrypted and restore/rollback steps are reviewed;
- [ ] exact candidate image, chart, and source digests are recorded in the GitHub issue;
- [ ] amd64 smoke runs for 10–15 minutes without restart, fatal error, or unexpected rotation;
- [ ] the arm64 image builds and its `--help` startup command succeeds;
- [ ] optional benchmark, if run, is labeled diagnostic only;
- [ ] a maintainer manually approves promotion through the protected GitHub Environment.
