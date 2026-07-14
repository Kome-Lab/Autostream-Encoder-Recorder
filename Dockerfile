FROM golang:1.26.5-trixie AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/autostream-encoder-recorder -ldflags="-s -w -X github.com/example/autostream-encoder-recorder/internal/version.Version=${VERSION} -X github.com/example/autostream-encoder-recorder/internal/version.Commit=${COMMIT} -X github.com/example/autostream-encoder-recorder/internal/version.BuildDate=${BUILD_DATE}" ./cmd/encoder-recorder

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --home-dir /var/lib/autostream autostream \
    && mkdir -p /var/lib/autostream/encoder-recorder /var/lib/autostream/archives \
    && chown -R 65532:65532 /var/lib/autostream
COPY --from=build /out/autostream-encoder-recorder /usr/local/bin/autostream-encoder-recorder
COPY --from=build /out/autostream-encoder-recorder /usr/local/bin/encoder-recorder
ENV AUTOSTREAM_NODE_CONFIG=/etc/autostream-encoder-recorder/config.yml
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/autostream-encoder-recorder"]
