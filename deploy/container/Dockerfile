# 可选容器镜像样例。在仓库根目录构建：docker build -f deploy/container/Dockerfile -t apa .
# 先初始化数据卷：
#   docker run --rm -i -v apa-data:/data apa init --password-stdin
# 再用宿主网络启动（Linux）。apa 仍只监听宿主回环，同机的 HTTPS 反向代理从回环访问它，
# apa 才信任 X-Forwarded-Proto。不要发布端口（-p）：经端口发布到达的请求源地址是网桥网关，
# 不是回环，apa 会拒绝来自本机之外的明文登录（设计 §14.13）。
#   docker run -d --network host -v apa-data:/data apa serve
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/apa ./cli/apa && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/apa /usr/local/bin/apa
COPY --from=build --chown=65532:65532 /data /data
ENV APA_HOME=/data
VOLUME /data
ENTRYPOINT ["/usr/local/bin/apa"]
CMD ["serve"]
