# syntax=docker/dockerfile:1
#
# One image, four binaries (api, worker, migrate, fincorectl). Compose picks
# which one runs. Build from the repository root:
#
#   docker build -f devops/docker/Dockerfile -t fincore:dev backend
#
# Stage 1 compiles static binaries; stage 2 is distroless: no shell, no
# package manager, runs as a non-root user (uid 65532).

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
# Dependencies first, so code changes reuse the module cache layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X github.com/nandini7486/fincore/backend/internal/platform/metrics.Version=${VERSION}" \
      -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate ./cmd/fincorectl

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
USER nonroot:nonroot
ENV HTTP_ADDR=:8080 METRICS_ADDR=:9090
EXPOSE 8080 9090
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=5 CMD ["/app/api", "healthcheck"]
ENTRYPOINT ["/app/api"]
