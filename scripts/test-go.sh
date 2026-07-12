#!/bin/sh
set -eu

if [ -z "${KUBEBUILDER_ASSETS:-}" ] || [ ! -x "$KUBEBUILDER_ASSETS/kube-apiserver" ]; then
	printf '%s\n' 'KUBEBUILDER_ASSETS must point to pinned envtest binaries' >&2
	exit 2
fi

# No Go test may discover the operator's current kubeconfig by accident.
KUBECONFIG=/dev/null
export KUBECONFIG
unset KUBERNETES_MASTER

exec go test "$@" ./...
