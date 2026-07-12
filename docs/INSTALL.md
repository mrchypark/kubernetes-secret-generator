# Installation

## Support and prerequisites

The `v4.0.0-rc.6` candidate targets Kubernetes 1.34/1.35 APIs, Helm 3.14+, amd64,
and an arm64 image build plus `--help` startup check. These are candidate targets, not an SLA, capacity, HA, or production
certification. The cluster should enforce Secret at-rest encryption and use a non-default
namespace.

Choose exactly one CRD manager for the lifetime of the release:

- `direct`: an operator applies CRDs with server-side apply before Helm.
- `flux`: Flux owns CRDs using `CreateReplace`; humans and direct Helm must not modify them.

Never let Helm, Flux, and a manual process compete for CRD ownership.

## Verify the release image

Copy the exact digest from the release evidence and verify it before deployment. See [RELEASE.md](RELEASE.md). Do not substitute a tag or `latest`.

## Direct Helm install

```sh
export KUBE_CONTEXT=my-cluster
export CONFIRM_CONTEXT="$KUBE_CONTEXT"
export NAMESPACE=secret-generator-system
export RELEASE_NAME=kubernetes-secret-generator
export CHART_VERSION=4.0.0-rc.6
export IMAGE_DIGEST='sha256:<verified-64-hex-digest>'
export CRD_LIFECYCLE_MANAGER=direct
export SCOPE_MODE=ownNamespace

kubectl --context "$KUBE_CONTEXT" auth can-i create customresourcedefinitions.apiextensions.k8s.io
make install
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" rollout status \
  deployment/"$RELEASE_NAME" --timeout=120s
```

`scripts/helm-release.sh` server-side-applies the packaged CRDs first and invokes exact `helm install --skip-crds`. A fresh install defaults to `ownNamespace` and requires the Helm release and all three product CRDs to be absent; scope confirmation is reserved for upgrade or the reviewed raw-v3 migration path. The script verifies the explicit context, optional `EXPECTED_SERVER_URL` and `EXPECTED_CA_SHA256`, namespace, chart version, CRD manager, and rendered digest. For a production install, start from the schema-checked and render-tested [production values example](examples/production-values.yaml), replacing its zero digest with the verified release digest.

The default `ownNamespace` scope is least privilege. For `namespaces` or `cluster`, complete the inventory and confirmation described in [MIGRATION-v4.md](MIGRATION-v4.md); changing scope is a separate approved change.

## Flux install

Existing Flux installations may use the indexed [Flux example](examples/flux-helmrelease.yaml) as a starting point. Replace its zero Git commit and image digest with the exact candidate; the Git source uses `reconcileStrategy: Revision`. Keep Flux as the sole CRD manager. Flux is optional and its rehearsal is not a universal rc.6 promotion blocker.

Choose replica and topology settings for the target cluster. The rc.6 release process does
not certify HA or make a PDB availability claim.

Commit the reviewed HelmRelease and immutable source revision through the installation's
normal GitOps process. Verify the three CRDs become `Established` before accepting the
manager rollout. Do not use direct `kubectl apply` for CRDs in a Flux-managed installation.
Flux compatibility is documented for existing consumers but is not exercised by the rc.6
release automation.

## Uninstall

```sh
KUBE_CONTEXT=my-cluster CONFIRM_CONTEXT=my-cluster \
NAMESPACE=secret-generator-system RELEASE_NAME=kubernetes-secret-generator \
make uninstall
```

CRDs and CR/Secret data are retained by policy. Export an encrypted backup before any deliberate CRD deletion.

## Reinstall after retained-data uninstall

Uninstall retains the lifecycle owner record together with CRDs and CR data. Reinstall only the same release and scope, with an explicit target confirmation:

```sh
export KUBE_CONTEXT=my-cluster
export CONFIRM_CONTEXT="$KUBE_CONTEXT"
export NAMESPACE=secret-generator-system
export RELEASE_NAME=kubernetes-secret-generator
export CHART_VERSION=4.0.0-rc.6
export IMAGE_DIGEST='sha256:<verified-64-hex-digest>'
export CRD_LIFECYCLE_MANAGER=direct
export EXPECTED_SERVER_URL='<exact-approved-api-server>'
export EXPECTED_CA_SHA256='<exact-approved-ca-sha256>'
export REINSTALL=true
export SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace
export CONFIRM_REINSTALL="$KUBE_CONTEXT/$NAMESPACE/$RELEASE_NAME"
make install
```

The wrapper requires exactly one matching retained `direct` owner record and all three marked CRDs. Missing/malformed evidence, a different scope, or Flux ownership fails before CRD mutation.

## Raw v3.4.1 migration

A raw-manifest v3 deployment is not a fresh install. First run `scripts/preflight-v4.sh` against the exact context/server/CA and retain its zero-blocker JSON or Markdown report in the GitHub release issue. Then use the explicit migration classification:

```sh
export KUBE_CONTEXT=my-cluster
export CONFIRM_CONTEXT="$KUBE_CONTEXT"
export NAMESPACE=secret-generator-system
export RELEASE_NAME=kubernetes-secret-generator
export CHART_VERSION=4.0.0-rc.6
export IMAGE_DIGEST='sha256:<verified-64-hex-digest>'
export CRD_LIFECYCLE_MANAGER=direct
export RAW_V3_MIGRATION=true
export SCOPE_MODE=cluster CONFIRMED_SCOPE=cluster
export CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1
export RAW_V3_PREFLIGHT_REPORT=/absolute/path/to/preflight-report.json
export RAW_V3_PREFLIGHT_SHA256='<sha256-of-that-report>'
export EXPECTED_SERVER_URL='<exact-approved-api-server>'
export EXPECTED_CA_SHA256='<exact-approved-ca-sha256>'
make install
```

The report must be no older than 24 hours and match the current context, server, CA, namespace, and release. The three unmarked CRDs must byte-match the pinned v3.4.1 normalized specs; partial or unknown CRD sets are rejected before write.
