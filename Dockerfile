# api / worker / migrate 共用镜像，通过 --mode 区分角色。
# 运行时用 alpine 而非 scratch：保留 shell 与 wget，便于 healthcheck 与现场排障。

FROM golang:1.27-alpine AS build
WORKDIR /src

# 先只拷贝依赖清单，让依赖层能够被缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kaohe ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget && mkdir -p /data/uploads

COPY --from=build /out/kaohe /app/kaohe

WORKDIR /app
EXPOSE 8080

ENTRYPOINT ["/app/kaohe"]
CMD ["--mode=api"]
