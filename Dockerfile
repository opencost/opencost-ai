# syntax=docker/dockerfile:1.7
#
# opencost-ai-gateway image.
#
# Two-stage build: a pinned golang:1.22 builder, then Google's
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

ARG GO_VERSION=1.22.12
ARG BUILDER_IMAGE=golang:${GO_VERSION}-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${BUILDER_IMAGE} AS build
WORKDIR /src

# Prime the module cache first so source edits don't bust it.
COPY go.mod ./
# go.sum is optional for a zero-dep module; COPY with a glob so the
# build does not fail before we have one.
COPY go.su[m] ./
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

# Build flags rationale:
#   CGO_DISABLED=0 keeps the binary static and lets distroless-static serve it.
#   -trimpath removes build-host paths from the binary (reproducibility).
#   -buildvcs=false keeps the .git tree out of the embedded build info,
#     because CI typically copies source without .git.
#   -ldflags "-s -w" strips symbol and DWARF tables to shrink the image;
#     main.version is stamped for /v1/version.
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
