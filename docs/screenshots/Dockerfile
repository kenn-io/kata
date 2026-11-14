# Every tool docs/screenshots/generate.sh needs, for hosts that have only
# Docker. Versions follow mise.toml, web/package.json and Freeze 0.2.2.
FROM golang:1.27.4-bookworm AS go

FROM mcr.microsoft.com/playwright:v1.64.1-noble
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    gcc \
    libc6-dev \
    libsqlite3-dev \
    make \
    python3 \
    tmux \
    unzip \
    && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
RUN case "$(dpkg --print-architecture)" in \
      arm64) BUN_ARCH=aarch64; FREEZE_ARCH=arm64 ;; \
      *) BUN_ARCH=x64; FREEZE_ARCH=x86_64 ;; \
    esac && \
    curl -fsSL "https://github.com/oven-sh/bun/releases/download/bun-v1.4.4/bun-linux-${BUN_ARCH}.zip" -o /tmp/bun.zip && \
    unzip -q /tmp/bun.zip -d /tmp && mv "/tmp/bun-linux-${BUN_ARCH}/bun" /usr/local/bin/bun && \
    curl -fsSL "https://github.com/charmbracelet/freeze/releases/download/v0.2.2/freeze_0.2.2_Linux_${FREEZE_ARCH}.tar.gz" | tar -xz -C /tmp && \
    mv /tmp/freeze_*/freeze /usr/local/bin/freeze && \
    rm -rf /tmp/bun* /tmp/freeze_* && \
    bun --version && freeze --version
