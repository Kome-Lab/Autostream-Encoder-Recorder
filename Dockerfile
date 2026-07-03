FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/encoder-recorder ./cmd/encoder-recorder

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --home-dir /var/lib/autostream autostream \
    && mkdir -p /var/lib/autostream/encoder-recorder /var/lib/autostream/archives \
    && chown -R 65532:65532 /var/lib/autostream
COPY --from=build /out/encoder-recorder /usr/local/bin/encoder-recorder
ENV AUTOSTREAM_NODE_CONFIG=/etc/autostream-node/config.yml
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/encoder-recorder"]
