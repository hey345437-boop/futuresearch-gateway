# 多阶段构建：产出 ~10MB 的单二进制镜像（面板已 embed 进去，运行时零依赖）。
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/fsgw ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 fsgw
WORKDIR /data
COPY --from=build /out/fsgw /usr/local/bin/futuresearch-gateway
USER fsgw
# 容器里必须监听 0.0.0.0 才能被映射出去 —— 所以**必须**同时设 api_key 与 admin_password，
# 否则启动时会拒绝（这是刻意的安全闸，别去改它）。
ENV FSGW_LISTEN_HOST=0.0.0.0
EXPOSE 7868
VOLUME ["/data"]
ENTRYPOINT ["futuresearch-gateway"]
CMD ["--config", "/data/config.json"]
