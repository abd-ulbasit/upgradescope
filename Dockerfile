# syntax=docker/dockerfile:1
# Multi-stage build: node builds the dashboard, Go embeds it, distroless
# runs it. One binary, three subcommands (scan/agent/serve); the chart
# picks the subcommand via args. `serve` exposes the dashboard at /.
#
# This is the from-source image (kind e2e, `make docker-build`). Published
# release images come from Dockerfile.release, which packages the binaries
# GoReleaser already built instead of compiling again.
#
# Every base is pinned by digest so a rebuild of the same commit uses the
# same bases; Dependabot (docker ecosystem) proposes digest bumps.
#
# The web and build stages run on the builder's own platform
# (--platform=$BUILDPLATFORM) and Go cross-compiles for the target, so
# `make images` (linux/amd64 + linux/arm64) needs no emulation: the dashboard
# bundle is platform-independent and the binary is static (CGO_ENABLED=0).

FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:0b36e8c136b94cd4fcf02188228e76c31ad5872eef3fec8cbd2eee500cfd9e80 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ .
RUN npm run build

# go.mod requires go1.26.8 and the official image sets GOTOOLCHAIN=local, so
# the builder must be at least that patch.
FROM --platform=$BUILDPLATFORM golang:1.26.8@sha256:0f063af2d465d8dcae54cce04278ada488b96f77b42449c8d071e47d016cc65a AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Stage the freshly built dashboard where go:embed picks it up (same as
# `make web`), replacing the committed copy: COPY merges, so clear it first
# or stale hashed assets would be embedded next to the new ones.
RUN find internal/server/webdist -mindepth 1 ! -name .gitkeep -delete
COPY --from=web /src/web/dist/ internal/server/webdist/
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath \
    -ldflags "-s -w -X github.com/abd-ulbasit/upgradescope/internal/cli.version=${VERSION}" \
    -o /out/upgradescope ./cmd/upgradescope

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/upgradescope /upgradescope
USER 65532:65532
ENTRYPOINT ["/upgradescope"]
