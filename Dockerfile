# Stage 1: Build
FROM golang:1.24-alpine AS builder

# 默认使用官方代理；国内网络请换成镜像源再构建：
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct -t weclawbot-api-webui:1.0.1 .
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy dependency files and download
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o weclawbot-api .

# Stage 2: Runtime
FROM alpine:latest

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy binary from builder
COPY --from=builder /app/weclawbot-api .

# 第三方组件声明必须随镜像一起分发。rsc.io/qr 是 BSD 3-Clause，其中一条明确要求
# 「以二进制形式再分发时，必须在随附的文档或材料中重现版权声明和免责声明」——
# 镜像就是二进制分发，声明文件缺了就是不合规。
COPY --from=builder /app/THIRD_PARTY_NOTICES.md .

# Create config directory and volume
RUN mkdir -p /app/config && \
    ln -s /app/weclawbot-api /usr/local/bin/weclawbot-api && \
    ln -s /app/weclawbot-api /usr/local/bin/bot

# Expose default API port
EXPOSE 26322

# Run
ENTRYPOINT ["/app/weclawbot-api"]
