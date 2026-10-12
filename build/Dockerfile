# Multi-architecture build with immutable base images.
FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS builder

WORKDIR /workdir
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown

COPY go.mod go.sum ./
RUN go mod download

COPY cmd cmd
COPY pkg pkg
COPY version version

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X=github.com/mittwald/kubernetes-secret-generator/version.Version=${VERSION}" \
    -o /workspace/manager ./cmd/manager/main.go

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
ARG VERSION=dev
ARG REVISION=unknown
ARG SOURCE=https://github.com/mrchypark/kubernetes-secret-generator

LABEL org.opencontainers.image.title="kubernetes-secret-generator" \
      org.opencontainers.image.description="Kubernetes controller for generated Secret values" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /
COPY --from=builder --chown=65532:65532 /workspace/manager /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
