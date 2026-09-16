# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG VERSION=dev
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/Claiyc/pelican-k8s/internal/version.Version=$VERSION" -o /out/agent ./cmd/agent

# Wings' archive code shells out to nothing, but time zone data and CA
# certificates are needed for TZ handling and S3 backups.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent
ENTRYPOINT ["/agent"]
