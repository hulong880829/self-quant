# syntax=docker/dockerfile:1
FROM golang:1.25 AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOBIN=/out

COPY go.mod go.sum ./
COPY third_party/hyperliquid-go ./third_party/hyperliquid-go
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out && \
    go build -trimpath -ldflags="-s -w" -o /out/funding-service ./cmd/funding-service && \
    go build -trimpath -ldflags="-s -w" -o /out/funding-history-backfill ./cmd/funding-history-backfill && \
    go build -trimpath -ldflags="-s -w" -o /out/account-service ./cmd/account-service && \
    go build -trimpath -ldflags="-s -w" -o /out/polymarket-service ./cmd/polymarket-service && \
    go build -trimpath -ldflags="-s -w" -o /out/report-service ./cmd/report-service && \
    go build -trimpath -ldflags="-s -w" -o /out/trader-service ./cmd/trader-service && \
    go build -trimpath -ldflags="-s -w" -o /out/ai-service ./cmd/ai-service && \
    go build -trimpath -ldflags="-s -w" -o /out/spread-service ./cmd/spread-service && \
    go build -trimpath -ldflags="-s -w" -o /out/aggdata-service ./cmd/aggdata-service && \
    go build -trimpath -ldflags="-s -w" -o /out/api-gateway ./cmd/api-gateway && \
    go install github.com/grpc-ecosystem/grpc-health-probe@v0.4.39

FROM busybox:1.37.0-musl AS busybox

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ /out/
COPY --from=busybox /bin/wget /wget

USER nonroot:nonroot
