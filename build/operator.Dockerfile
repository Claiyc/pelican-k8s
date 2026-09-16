# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG VERSION=dev
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/Claiyc/pelican-k8s/internal/version.Version=$VERSION" -o /out/operator ./cmd/operator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/operator /operator
ENTRYPOINT ["/operator"]
