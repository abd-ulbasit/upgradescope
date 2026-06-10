# syntax=docker/dockerfile:1
# Multi-stage build: static Go binary -> distroless. One binary, three
# subcommands (scan/agent/serve); the chart picks the subcommand via args.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/abd-ulbasit/upgradescope/internal/cli.version=${VERSION}" \
    -o /out/upgradescope ./cmd/upgradescope

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/upgradescope /upgradescope
USER 65532:65532
ENTRYPOINT ["/upgradescope"]
