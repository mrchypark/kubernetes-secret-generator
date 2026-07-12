SHELL := /bin/sh

VERSION ?= dev
IMAGE_TAG_BASE ?= ghcr.io/mrchypark/kubernetes-secret-generator
IMG ?= $(IMAGE_TAG_BASE):$(VERSION)

CHART_DIR ?= deploy/helm-chart/kubernetes-secret-generator
RELEASE_NAME ?= kubernetes-secret-generator
ENVTEST_K8S_VERSION ?= 1.35.0
SETUP_ENVTEST_VERSION ?= v0.24.1
ENVTEST_DIR ?= $(CURDIR)/.cache/envtest
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
CONTROLLER_GEN_DIR ?= $(CURDIR)/.cache/tools
CONTROLLER_GEN ?= $(CONTROLLER_GEN_DIR)/controller-gen
COVERAGE_PROFILE ?= c.out
FUZZ_TIME ?= 30s
FUZZ_TEST_TIMEOUT ?= 2m

.PHONY: install upgrade uninstall
install upgrade:
	CHART_DIR="$(CHART_DIR)" RELEASE_NAME="$(RELEASE_NAME)" scripts/helm-release.sh "$@"

uninstall:
	RELEASE_NAME="$(RELEASE_NAME)" scripts/helm-release.sh uninstall

.PHONY: test test-unit test-race test-fuzz test-fuzz-one test-coverage test-envtest-assets test-source test-safety test-shell test-supply-chain test-helm test-docs test-kind-foundation test-kind-release test-kind-benchmark preflight backup restore
test: test-source test-unit test-safety test-supply-chain test-helm

test-envtest-assets:
	mkdir -p "$(ENVTEST_DIR)"
	$(SETUP_ENVTEST) use --bin-dir "$(ENVTEST_DIR)" -p path "$(ENVTEST_K8S_VERSION)" >/dev/null

test-unit: test-envtest-assets
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use --bin-dir "$(ENVTEST_DIR)" -i -p path "$(ENVTEST_K8S_VERSION)")" \
		scripts/test-go.sh -count=1 -shuffle=on

test-race: test-envtest-assets
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use --bin-dir "$(ENVTEST_DIR)" -i -p path "$(ENVTEST_K8S_VERSION)")" \
		scripts/test-go.sh -count=1 -race -shuffle=on

test-fuzz: test-envtest-assets
	@status=0; \
	for target in FuzzParseByteLength FuzzValidateEncoding FuzzParseRegenerate FuzzPrivateKeyPEM; do \
		KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use --bin-dir "$(ENVTEST_DIR)" -i -p path "$(ENVTEST_K8S_VERSION)")" \
		FUZZ_TARGET="$$target" FUZZ_TIME="$(FUZZ_TIME)" FUZZ_TEST_TIMEOUT="$(FUZZ_TEST_TIMEOUT)" scripts/test-fuzz.sh || status=1; \
	done; \
	exit $$status

test-fuzz-one: test-envtest-assets
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use --bin-dir "$(ENVTEST_DIR)" -i -p path "$(ENVTEST_K8S_VERSION)")" \
		FUZZ_TARGET="$(FUZZ_TARGET)" FUZZ_TIME="$(FUZZ_TIME)" FUZZ_TEST_TIMEOUT="$(FUZZ_TEST_TIMEOUT)" FUZZ_ARTIFACT_DIR="$(FUZZ_ARTIFACT_DIR)" scripts/test-fuzz.sh

test-coverage: test-envtest-assets
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use --bin-dir "$(ENVTEST_DIR)" -i -p path "$(ENVTEST_K8S_VERSION)")" \
		scripts/test-go.sh -count=1 -covermode=set -coverprofile=c.raw.out -coverpkg=./...
	scripts/merge-coverprofile.sh c.raw.out "$(COVERAGE_PROFILE)"
	rm -f c.raw.out
	scripts/check-coverage.sh "$(COVERAGE_PROFILE)"

test-source: verify-generated
	go mod verify
	CONTROLLER_GEN_INSTALL_DIR="$(CONTROLLER_GEN_DIR)" scripts/test-controller-gen-lock.sh
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" || { \
		gofmt -l $$(find . -name '*.go' -not -path './vendor/*'); \
		exit 1; \
	}
	go vet ./...

test-safety: test-shell
	(cd scripts/check-test-artifacts-src && go test ./...)
	scripts/test-check-coverage.sh
	scripts/verify-kind-target-guards.sh
	test/safety.sh

test-shell:
	@for file in $$(find scripts test -type f -name '*.sh' | sort); do sh -n "$$file"; done
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck $$(find scripts test -type f -name '*.sh' | sort); fi

test-supply-chain:
	scripts/test-supply-chain-static.sh
	scripts/test-vulnerability-waivers.sh
	scripts/test-release-contract.sh
	scripts/test-preflight-v4.sh
	scripts/test-backup-restore.sh

test-helm:
	scripts/verify-helm-chart.sh
	scripts/verify-helm-release-guards.sh
	scripts/verify-n1-crd-fixtures.sh

test-docs: test-envtest-assets
	@test -n "$(DOC_EXAMPLES_HELM)" && test -x "$(DOC_EXAMPLES_HELM)" || { echo 'DOC_EXAMPLES_HELM must be the absolute locked Helm executable' >&2; exit 2; }
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use --bin-dir "$(ENVTEST_DIR)" -i -p path "$(ENVTEST_K8S_VERSION)")" \
		KUBECONFIG=/dev/null DOC_EXAMPLES_HELM="$(DOC_EXAMPLES_HELM)" \
		go test ./test/envtest -count=1 -run '^TestDocumentationExamplesServerDryRun$$'
test-kind-foundation:
	test/e2e/kind-foundation.sh

test-kind-release:
	test/e2e/release-smoke.sh

test-kind-benchmark:
	test/e2e/benchmark.sh

preflight:
	scripts/preflight-v4.sh

backup restore:
	scripts/backup-restore.sh "$@"

.PHONY: controller-gen generate manifests verify-generated
controller-gen:
	CONTROLLER_GEN_INSTALL_DIR="$(CONTROLLER_GEN_DIR)" scripts/install-controller-gen.sh

generate: controller-gen
	CONTROLLER_GEN="$(CONTROLLER_GEN)" scripts/generate.sh object

manifests: controller-gen
	CONTROLLER_GEN="$(CONTROLLER_GEN)" scripts/generate.sh manifests

verify-generated: controller-gen
	CONTROLLER_GEN="$(CONTROLLER_GEN)" scripts/generate.sh verify

.PHONY: fmt build
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

build:
	docker build -f build/Dockerfile --build-arg VERSION="$(VERSION)" -t "$(IMG)" .
