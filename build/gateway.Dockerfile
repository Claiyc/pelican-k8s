# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:738d1cf061836894ff6bb8c33881080ac66de8cf0586615012a0c8f592649cfa AS build
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
    -ldflags "-s -w -X github.com/Claiyc/pelican-k8s/internal/version.Version=$VERSION" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/gateway /gateway
ENTRYPOINT ["/gateway"]
