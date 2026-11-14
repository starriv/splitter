# Multi-stage build producing a static binary on a distroless runtime image.
# The runtime image contains no shell and no package manager, and runs as a non-root user.
# Every listening port is above 1024, so the container requires no Linux capabilities.
#
# The build stage runs on the build host's platform and cross-compiles for the target
# platform (BuildKit sets TARGETOS and TARGETARCH), so multi-platform images build without
# emulation. Released images are built by .github/workflows/release.yml; a local
# `docker build .` produces an image for the host platform.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/splitter ./cmd/splitter
# The runtime image has no shell to create directories, so the log directory is
# made here and copied with the runtime user's ownership.
RUN mkdir -p /out/log

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="splitter" \
      org.opencontainers.image.description="DNS query splitter for front-end resolvers" \
      org.opencontainers.image.source="https://github.com/starriv/splitter" \
      org.opencontainers.image.licenses="GPL-3.0" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/splitter /usr/local/bin/splitter
COPY --from=build /src/LICENSE /usr/share/licenses/splitter/LICENSE
# Default log.dir: the query log is written here. Mount a volume to keep it
# across container re-creation.
COPY --from=build --chown=65532:65532 /out/log /var/log/splitter
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/splitter"]
CMD ["-config", "/etc/splitter/config.yaml"]

# The runtime image provides neither a shell nor an HTTP client, so the binary performs its
# own health check: it queries the admin /readyz endpoint and sends one UDP query to every
# DNS address the process has bound. A failed check indicates either that the most recent
# configuration was rejected (the previous runtime continues serving) or that the DNS
# listener is not responding. Docker reports the result in `docker ps` without restarting
# the container.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/splitter", "-healthcheck", "-config", "/etc/splitter/config.yaml"]
