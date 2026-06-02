FROM golang:1.26.3-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/kata ./cmd/kata

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates curl jq \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/kata /usr/local/bin/kata
COPY docker/federation/runner.sh /usr/local/bin/federation-smoke

ENTRYPOINT ["/usr/local/bin/kata"]
