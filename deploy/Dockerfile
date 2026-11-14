# 多阶段构建：最终镜像只含二进制与配置，不含源码与编译工具链。

# 运行时基础镜像可替换：默认 distroless（无 shell、无包管理器、攻击面最小），
# 但 gcr.io 在部分网络环境拉不动。内网/离线环境改成私有仓库里的等价镜像：
#   docker build --build-arg RUNTIME_IMAGE=registry.local/base/alpine:3.20 ...
# 换成 alpine 时注意补 CA 证书，否则 HTTPS 的 webhook 会握手失败。
# ARG 必须声明在第一个 FROM 之前，否则 FROM 里取不到值。
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM golang:1.25-alpine AS build
# 模块代理可替换：默认官方源，国内/内网构建时指定镜像源，避免卡在 pull 阶段。
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct ...
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 三个二进制共用一次编译缓存；-tags duckdb 需要 cgo，默认不开启以保持静态镜像。
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 GOOS=linux go build -tags "$BUILD_TAGS" -trimpath -ldflags="-s -w" -o /out/godw-server ./cmd/godw-server \
 && CGO_ENABLED=0 GOOS=linux go build -tags "$BUILD_TAGS" -trimpath -ldflags="-s -w" -o /out/godw-worker ./cmd/godw-worker \
 && CGO_ENABLED=0 GOOS=linux go build -tags "$BUILD_TAGS" -trimpath -ldflags="-s -w" -o /out/godw-cli ./cmd/godw-cli

FROM ${RUNTIME_IMAGE}
WORKDIR /app
COPY --from=build /out/godw-server /app/godw-server
COPY --from=build /out/godw-worker /app/godw-worker
COPY --from=build /out/godw-cli /app/godw-cli
COPY config.example.yaml /app/config.yaml
# 用数字 UID：distroless 的 nonroot 就是 65532，alpine 里没有该用户名但数字有效，
# 两种基础镜像都能跑同一份 Dockerfile。
USER 65532:65532
# 容器规范：日志走 stdout，健康检查走 HTTP，SIGTERM 触发优雅退出。
EXPOSE 8080
ENTRYPOINT ["/app/godw-server"]
CMD ["--config", "/app/config.yaml"]
