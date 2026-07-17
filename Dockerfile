# ==========================================
# Build Stage
# ==========================================
FROM golang:1.21-bookworm AS builder

# Install build dependencies for CGO and libopus
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libopus-dev \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy dependency files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the application with CGO enabled (required for opus bindings)
RUN CGO_ENABLED=1 GOOS=linux go build -o pinstrel .

# ==========================================
# Runtime Stage
# ==========================================
FROM debian:bookworm-slim

# Install runtime dependencies:
# - shairport-sync for AirPlay capture
# - libopus0 for Opus audio encoding
# - avahi-daemon for AirPlay mDNS discovery
# - ca-certificates for secure Discord API communication
RUN apt-get update && apt-get install -y --no-install-recommends \
    shairport-sync \
    libopus0 \
    avahi-daemon \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Configure avahi-daemon to run without D-Bus in the container
RUN sed -i 's/^[#]*enable-dbus=.*/enable-dbus=no/' /etc/avahi/avahi-daemon.conf

# Copy binary and configuration files
COPY --from=builder /app/pinstrel /usr/local/bin/pinstrel
COPY --from=builder /app/shairport-sync.conf.template /etc/shairport-sync.conf.template
COPY --from=builder /app/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# Ensure the entrypoint script is executable
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Default environment variables
ENV DISCORD_TOKEN="" \
    DISCORD_CHANNEL_ID="" \
    BITRATE=128000 \
    PIPE_PATH="/tmp/shairport-sync-audio" \
    SOCKET_PATH="/tmp/pinstrel.sock"

# Expose default AirPlay ports
# Note: For reliable mDNS/AirPlay discovery, running the container
# with host networking (--net=host) is highly recommended.
EXPOSE 5000/tcp 5353/udp 6001/udp 6002/udp 6003/udp

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
