#!/bin/sh
set -eu

identity=${1:?usage: verify-signing-identity.sh IDENTITY ISSUER RELEASE_TAG}
issuer=${2:?usage: verify-signing-identity.sh IDENTITY ISSUER RELEASE_TAG}
tag=${3:?usage: verify-signing-identity.sh IDENTITY ISSUER RELEASE_TAG}
expected="https://github.com/mrchypark/kubernetes-secret-generator/.github/workflows/release-candidate.yml@refs/tags/$tag"
[ "$identity" = "$expected" ] || { echo 'certificate identity does not match the exact release workflow tag' >&2; exit 1; }
[ "$issuer" = 'https://token.actions.githubusercontent.com' ] || { echo 'certificate issuer is not GitHub Actions OIDC' >&2; exit 1; }
