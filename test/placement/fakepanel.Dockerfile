# syntax=docker/dockerfile:1
# The fake Panel of the live placement suite (hack/e2e-placement.sh).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    for i in 1 2 3; do go mod download && exit 0; echo "go mod download failed (attempt $i), retrying" >&2; sleep 5; done; exit 1
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/fakepanel ./test/fakepanel/cmd/fakepanel

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/fakepanel /fakepanel
ENTRYPOINT ["/fakepanel"]
