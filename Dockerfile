# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26.3-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/evolt-ocpi-partner-console ./cmd/server

FROM debian:bookworm-slim AS runtime

WORKDIR /app

ENV TZ=Asia/Bangkok
ENV PORT=8080

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --shell /usr/sbin/nologin evoltuser

# Pages and assets are embedded in the binary, nothing else to copy.
COPY --from=builder --chown=evoltuser:evoltuser /out/evolt-ocpi-partner-console /app/evolt-ocpi-partner-console

USER evoltuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD curl --fail --silent --show-error "http://127.0.0.1:${PORT:-8080}/health" || exit 1

CMD ["/app/evolt-ocpi-partner-console"]
