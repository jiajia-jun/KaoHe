# api / worker / migrate 共用镜像，通过 --mode 区分角色。
# 运行时用 alpine 而非 scratch：保留 shell 与 wget，便于 healthcheck 与现场排障。

FROM golang:1.27-alpine AS build
WORKDIR /src

# docker build 不继承宿主机的 GOPROXY，默认会去打 proxy.golang.org。
# 这一行是为了让构建在国内网络下也能拉到模块 —— 少了它，
# 第一次 docker compose up --build 会在一台 go 命令完全正常的机器上失败。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

# 先只拷贝依赖清单，让依赖层能够被缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kaohe ./cmd/server

FROM alpine:3.21
# /data/logs 平时由 compose 的绑定挂载盖住，这里显式建出来是为了兜住
# 「不用 compose、直接跑二进制」的情况 —— lumberjack 自己也会 MkdirAll，
# 但日志目录不可写时会直接拒绝启动，提前建好能少一类启动失败。
RUN apk add --no-cache ca-certificates wget && mkdir -p /data/uploads /data/logs

COPY --from=build /out/kaohe /app/kaohe

WORKDIR /app
EXPOSE 8080

ENTRYPOINT ["/app/kaohe"]
CMD ["--mode=api"]
