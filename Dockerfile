# syntax=docker/dockerfile:1

FROM golang:1.27-bookworm AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agy-api ./cmd/agy-api

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    gzip \
    procps \
    tar \
  && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --home-dir /home/agy --uid 1000 --shell /bin/bash agy \
  && mkdir -p /home/agy/.gemini /home/agy/.local/bin /workspace \
  && chown -R agy:agy /home/agy /workspace

COPY --from=builder /out/agy-api /usr/local/bin/agy-api

USER agy
ENV HOME=/home/agy \
    PATH="/home/agy/.local/bin:/usr/local/bin:/usr/bin:/bin" \
    PORT=8080 \
    AGY_ACP_BIN=/home/agy/.local/bin/agy \
    AGY_ACP_ARGS=acp \
    AGY_WORKSPACE=/workspace \
    AGY_PERMISSION_POLICY=allow_all

RUN curl -fsSL https://antigravity.google/cli/install.sh | bash -s -- --dir /home/agy/.local/bin \
  && /home/agy/.local/bin/agy --version

VOLUME ["/home/agy/.gemini"]
WORKDIR /workspace
EXPOSE 8080

CMD ["/usr/local/bin/agy-api"]
