# syntax=docker/dockerfile:1
# Multi-stage build: node builds the dashboard, Go embeds it, distroless
# runs it. One binary, three subcommands (scan/agent/serve); the chart
# picks the subcommand via args. `serve` exposes the dashboard at /.

FROM node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ .
RUN npm run build

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Stage the built dashboard where go:embed picks it up (same as `make web`).
COPY --from=web /src/web/dist/ internal/server/webdist/
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
