# Daemon image: static Go binary + Litestream on Alpine (a shell is needed
# for the entrypoint, ca-certificates for TLS to Tigris).

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/chuck ./cmd/chuck

FROM alpine:3.24 AS litestream
ARG TARGETARCH
ARG LITESTREAM_VERSION=0.5.17
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) arch=x86_64; sum=cfb371176d164437ae869f8351cfde49bd1804ae71c61923f75c9cba9c9c006d ;; \
      arm64) arch=arm64;  sum=f8ca4a050095c1efbda2c4365172e61bf9d955ea0d9ac42f448b52e51819baa5 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    tgz="litestream-${LITESTREAM_VERSION}-linux-${arch}.tar.gz"; \
    wget -q "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/${tgz}"; \
    echo "${sum}  ${tgz}" | sha256sum -c -; \
    tar -xzf "$tgz" -C /usr/local/bin litestream

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=build /out/chuck /usr/local/bin/chuck
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY config /app/config
COPY deploy/litestream.yml /etc/litestream.yml
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
ENV CHUCK_CONFIG_DIR=/app/config \
    CHUCK_DB_PATH=/data/chuck.db \
    PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
