# Multi-stage build for the discord-dnd-bot services.
#
# Both `gateway` and `worker` are built from the same module. The gateway needs
# CGO (libopus, via layeh.com/gopus) to decode Discord voice; we therefore build
# with CGO enabled and copy the shared libopus runtime into the final image.
#
# Build a specific service with:
#   docker build --build-arg SERVICE=gateway -t discord-dnd-bot-gateway .
#   docker build --build-arg SERVICE=worker  -t discord-dnd-bot-worker  .

# ---- build stage ----
FROM golang:1.24-bookworm AS build

# libopus is required to compile/link gopus.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libopus-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Build.
COPY . .
ARG SERVICE=gateway
ARG VERSION=dev
ENV CGO_ENABLED=1
RUN go build -buildvcs=false \
    -ldflags "-s -w" \
    -o /out/app ./cmd/${SERVICE}

# ---- runtime stage ----
FROM debian:bookworm-slim AS runtime

# Runtime deps: libopus (for the gateway's voice decoding) and CA certs (TLS to
# Discord/LiteLLM/S3). ca-certificates keeps HTTPS working; tzdata for logs.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libopus0 ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin appuser

COPY --from=build /out/app /usr/local/bin/app

USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/app"]
