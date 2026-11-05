# 可选容器镜像样例。在仓库根目录构建：docker build -f deploy/container/Dockerfile -t apa .
# 先初始化数据卷：
#   docker run --rm -i -v apa-data:/data -v "$PWD/deploy/container/config.yaml:/data/config.yaml:ro" apa init --password-stdin
# 再启动，只把端口发布到宿主回环，再由宿主的反向代理做 HTTPS：
#   docker run -d -v apa-data:/data -v "$PWD/deploy/container/config.yaml:/data/config.yaml:ro" -p 127.0.0.1:7777:7777 apa serve
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/apa ./cli/apa && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/apa /usr/local/bin/apa
COPY --from=build --chown=65532:65532 /data /data
ENV APA_HOME=/data
VOLUME /data
EXPOSE 7777
ENTRYPOINT ["/usr/local/bin/apa"]
CMD ["serve"]
