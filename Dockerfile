# syntax=docker/dockerfile:1.7
#
# opencost-ai-gateway image.
#
# Two-stage build: a golang:1.26 builder, then Google's
# gcr.io/distroless/static-debian12:nonroot runtime. The runtime image
# ships UID 65532 ("nonroot") by default and contains no shell or
# package manager; the gateway is a statically linked binary and has
# no runtime dependencies beyond TLS roots, which distroless provides.
#
# The resulting image is expected to run with readOnlyRootFilesystem:
# the gateway writes only to stdout/stderr. Any future on-disk
# scratch usage must be mounted as an emptyDir with explicit tmpfs
# sizing — do not relax the read-only root.

# --- build stage --------------------------------------------------------------

# go.mod and this build pin the same Go line: current stable (1.26).
# 1.26 is a floating tag for the latest 1.26 patch so the shipped
# binary does not inherit unpatched stdlib CVEs from a stale point
# release. Release builds override via
# --build-arg GO_VERSION=<exact-patch> to pin deterministically.
ARG GO_VERSION=1.26
ARG BUILDER_IMAGE=golang:${GO_VERSION}-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${BUILDER_IMAGE} AS build
WORKDIR /src

# Prime the module cache first so source edits don't bust it. A
# zero-dep module has no go.sum, and a bracket-glob COPY is not
# portable (legacy Docker build errors on zero matches while BuildKit
# tolerates it), so we COPY go.mod alone. Once a dependency lands,
# add `COPY go.sum ./` and re-enable `go mod verify` on the next
# line.
COPY go.mod ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

# Build flags rationale:
#   CGO_ENABLED=0 keeps the binary static and lets distroless-static serve it.
#   -trimpath removes build-host paths from the binary (reproducibility).
#   -buildvcs=false keeps the .git tree out of the embedded build info,
#     because CI typically copies source without .git.
#   -ldflags "-s -w" strips symbol and DWARF tables to shrink the image;
#     main.version is stamped into the binary for build/version metadata
#     (logged on startup today; exposed via a dedicated endpoint in a
#     follow-up PR).
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
RUN go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/gateway \
      ./cmd/gateway

# --- runtime stage ------------------------------------------------------------

FROM ${RUNTIME_IMAGE}

# OCI labels. Image tag and git SHA are passed by CI; keep defaults
# here inert so a local "docker build ." still succeeds.
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="opencost-ai-gateway" \
      org.opencontainers.image.description="Thin HTTP gateway in front of ollama-mcp-bridge for OpenCost MCP tooling." \
      org.opencontainers.image.source="https://github.com/opencost/opencost-ai" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="OpenCost" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/gateway /usr/local/bin/gateway

# Distroless "nonroot" sets USER 65532:65532 already; declared again
# for operators inspecting the image and to make the non-root posture
# explicit per CLAUDE.md non-negotiables.
USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/gateway"]
