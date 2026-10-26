FROM --platform=$BUILDPLATFORM oven/bun:1.4.4 AS bun

# The web bundle is platform-independent, so build it once on the builder.
FROM --platform=$BUILDPLATFORM node:26.12.1-bookworm AS web
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun

WORKDIR /src
COPY package.json bun.lock ./
COPY web/package.json web/
COPY packages/kata-ui/package.json packages/kata-ui/
# @kenn-io/kit-ui is a commit-pinned public git dependency; git ships in the
# node:bookworm image.
RUN bun install --frozen-lockfile

COPY . .
RUN cd web && bun run build && bun run scripts/validate-assets.ts dist

FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build

# "|" falls back to the source repo on any proxy error, including HTTP/2 stream resets mid-download.
ENV GOPROXY="https://proxy.golang.org|direct"

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN rm -rf internal/web/dist
COPY --from=web /src/web/dist internal/web/dist

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X go.kenn.io/kata/internal/version.Version=${VERSION} -X go.kenn.io/kata/internal/version.Commit=${COMMIT} -X go.kenn.io/kata/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/kata ./cmd/kata

FROM alpine:3.24.2

RUN apk add --no-cache ca-certificates \
    && addgroup -g 1000 kata \
    && adduser -D -u 1000 -G kata kata \
    && mkdir /data \
    && chown 1000:1000 /data

COPY --from=build /out/kata /usr/local/bin/kata

ENV KATA_HOME=/data
WORKDIR /data
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/kata"]
CMD ["daemon", "start", "--foreground"]
