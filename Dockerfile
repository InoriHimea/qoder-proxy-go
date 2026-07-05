# Build Stage
FROM golang:1.25-alpine AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o qoder-proxy-go .

# Runtime Stage
FROM node:20-slim
WORKDIR /app

# node:20-slim ships without a CA bundle, which breaks TLS verification
# for all outbound HTTPS (OAuth polling, token refresh, etc.)
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install qodercli and qoderclicn globally (needed by the Go proxy)
RUN npm install -g @qoder-ai/qodercli @qodercn-ai/qoderclicn

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
