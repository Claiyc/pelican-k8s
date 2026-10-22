# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
ARG VERSION=dev
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# proxy.golang.org now and then resets a stream mid-download; finished modules
# stay in the cache mount, so a retry only fetches what failed.
RUN --mount=type=cache,target=/go/pkg/mod \
    for i in 1 2 3; do go mod download && exit 0; echo "go mod download failed (attempt $i), retrying" >&2; sleep 5; done; exit 1
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/Claiyc/pelican-k8s/internal/version.Version=$VERSION" -o /out/shim ./cmd/shim

# The shim image only carries the static binary; the operator copies it into
# game pods with the prepare init container.
FROM scratch
COPY --from=build /out/shim /shim
USER 65534:65534
ENTRYPOINT ["/shim"]
