# Build Stage
FROM golang:1.25-alpine AS builder
ENV GOPROXY=https://goproxy.cn,direct
ENV _SQLITE_EXT_RS_LOAD=disable
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o qoder-proxy-go .

# Runtime Stage
FROM node:20-slim
WORKDIR /app

ENV _SQLITE_EXT_RS_LOAD=disable

# node:20-slim ships without a CA bundle, which breaks TLS verification
# for all outbound HTTPS (OAuth polling, token refresh, etc.)
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Pin both CLIs so image rebuilds remain reproducible across npm releases.
ARG QODERCLI_VERSION=1.0.37
ARG QODERCLICN_VERSION=1.0.37
RUN npm install -g \
    @qoder-ai/qodercli@${QODERCLI_VERSION} \
    @qodercn-ai/qoderclicn@${QODERCLICN_VERSION} \
    && test "$(qodercli --version)" = "${QODERCLI_VERSION}" \
    && test "$(qoderclicn --version)" = "${QODERCLICN_VERSION}"

# Disable auto-updates
RUN mkdir -p /root/.qoder && echo '{"autoUpdates":false}' > /root/.qoder.json

# Copy Go binary and static assets
COPY --from=builder /build/qoder-proxy-go .
COPY --from=builder /build/write_token.mjs .
COPY public/ ./public/

# Create data directory
RUN mkdir -p /app/data

EXPOSE 3000
CMD ["./qoder-proxy-go"]
