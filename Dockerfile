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

FROM node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ .
RUN npm run build

# go.mod requires go1.26.8 and the official image sets GOTOOLCHAIN=local, so
# the builder must be at least that patch.
FROM golang:1.27.0@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146 AS build
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
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/abd-ulbasit/upgradescope/internal/cli.version=${VERSION}" \
    -o /out/upgradescope ./cmd/upgradescope

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/upgradescope /upgradescope
USER 65532:65532
ENTRYPOINT ["/upgradescope"]
