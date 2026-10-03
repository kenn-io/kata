FROM alpine:3.24.2

RUN apk add --no-cache ca-certificates \
    && addgroup -g 1000 kata \
    && adduser -D -u 1000 -G kata kata \
    && mkdir /data \
    && chown 1000:1000 /data

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/kata /usr/local/bin/kata

ENV KATA_HOME=/data
WORKDIR /data
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/kata"]
CMD ["daemon", "start", "--foreground"]
