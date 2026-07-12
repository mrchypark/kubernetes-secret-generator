---
name: Release checklist
about: Manual approval for an OSS-scale release promotion
title: "Release vX.Y.Z"
labels: release
---

Candidate run: <!-- exact https://github.com/OWNER/REPO/actions/runs/RUN_ID -->
Tag: `vX.Y.Z`
Source commit: <!-- 40 hex -->
Image digest: `sha256:`
Chart SHA-256: `sha256:`
Preflight report run: <!-- same exact candidate run URL; sanitized Markdown is in its artifact -->

- [ ] CI passed on the release commit
- [ ] Candidate tag is protected and matches Chart.yaml and values.yaml
- [ ] amd64 and arm64 images built from one candidate build and passed startup smoke
- [ ] Per-architecture vulnerability scans and SBOMs were reviewed
- [ ] The exact candidate digest has a valid keyless signature
- [ ] The disposable-kind upgrade/rollback smoke completed within 15 minutes
- [ ] v3-to-v4 preflight reported zero blockers and its safe Markdown report is linked above
- [ ] Backup/restore was manually rehearsed
- [ ] Release notes and upgrade/rollback documentation were reviewed

Close this issue only after every item is checked. The `production-release` Environment supplies the independent reviewer approval. Pass this issue's number or URL to the manual promotion workflow.
