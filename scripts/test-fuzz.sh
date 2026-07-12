#!/bin/sh
set -eu

if [ -z "${KUBEBUILDER_ASSETS:-}" ] || [ ! -x "$KUBEBUILDER_ASSETS/kube-apiserver" ]; then
	printf '%s\n' 'KUBEBUILDER_ASSETS must point to pinned envtest binaries' >&2
	exit 2
fi

target=${FUZZ_TARGET:-}
case "$target" in
	FuzzParseByteLength|FuzzValidateEncoding|FuzzParseRegenerate|FuzzPrivateKeyPEM) ;;
	*)
		printf '%s\n' 'FUZZ_TARGET must name one approved fuzz target' >&2
		exit 2
		;;
esac

fuzz_time=${FUZZ_TIME:-30s}
test_timeout=${FUZZ_TEST_TIMEOUT:-2m}
artifact_dir=${FUZZ_ARTIFACT_DIR:-.artifacts/fuzz/$target}
rm -rf "$artifact_dir"
mkdir -p "$artifact_dir"
KUBECONFIG=/dev/null
export KUBECONFIG
unset KUBERNETES_MASTER

status=0
go test -timeout="$test_timeout" -run='^$' -fuzz="^$target$" -fuzztime="$fuzz_time" ./pkg/controller/secret >"$artifact_dir/go-test.log" 2>&1 || status=$?
corpus="pkg/controller/secret/testdata/fuzz/$target"
if [ -d "$corpus" ]; then
	cp -R "$corpus" "$artifact_dir/corpus"
fi
exit "$status"
