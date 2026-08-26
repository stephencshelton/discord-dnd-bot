# Devcontainer image for discord-dnd-bot (Go bot with Discord voice capture).
# Voice receive decodes Opus via layeh.com/gopus (cgo -> needs libopus + pkg-config).
FROM mcr.microsoft.com/devcontainers/go:1-1.23-bookworm

# The base image ships a Yarn apt source (dl.yarnpkg.com) whose signing key has
# rotated, so `apt-get update` fails with:
#   NO_PUBKEY 62D54FD4003F6525 / "The repository ... is not signed"
# That breaks not just our RUN below but every later dev container Feature that
# runs apt-get (e.g. docker-in-docker). Remove the stale Yarn source so apt is
# healthy for us and for all Features installed afterwards.
RUN rm -f /etc/apt/sources.list.d/yarn.list \
    && rm -f /usr/share/keyrings/yarn*.gpg /etc/apt/keyrings/yarn*.gpg 2>/dev/null || true

# System deps:
#  - libopus/libopusfile + pkg-config: required by gopus (cgo) for voice decoding
#  - ffmpeg: audio muxing/conversion for recorded PCM -> wav/ogg before STT
#  - build-essential: cgo compiler toolchain
RUN apt-get update \
    && export DEBIAN_FRONTEND=noninteractive \
    && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        libopus-dev \
        libopusfile-dev \
        ffmpeg \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
