#!/bin/bash
set -e

# Generate /etc/pinstrel.toml from environment variables if not already present
if [ ! -f /etc/pinstrel.toml ]; then
  if [ -n "$DISCORD_TOKEN" ] && [ -n "$DISCORD_CHANNEL_ID" ]; then
    echo "Generating /etc/pinstrel.toml from environment variables..."
    cat <<EOF > /etc/pinstrel.toml
DISCORD_TOKEN = "${DISCORD_TOKEN}"
DISCORD_CHANNEL_ID = "${DISCORD_CHANNEL_ID}"
BITRATE = ${BITRATE:-128000}
PIPE_PATH = "${PIPE_PATH:-/tmp/shairport-sync-audio}"
SOCKET_PATH = "${SOCKET_PATH:-/tmp/pinstrel.sock}"
EOF
  else
    echo "ERROR: /etc/pinstrel.toml not found, and DISCORD_TOKEN/DISCORD_CHANNEL_ID environment variables are not set."
    exit 1
  fi
else
  echo "Using existing /etc/pinstrel.toml config file."
fi

# Ensure Shairport Sync configuration exists
if [ ! -f /etc/shairport-sync.conf ]; then
  if [ -f /etc/shairport-sync.conf.template ]; then
    echo "Using default shairport-sync.conf.template..."
    cp /etc/shairport-sync.conf.template /etc/shairport-sync.conf
  else
    echo "ERROR: /etc/shairport-sync.conf not found and template is missing."
    exit 1
  fi
fi

# Set up avahi-daemon runtime environment
mkdir -p /var/run/avahi-daemon
chown -R avahi:avahi /var/run/avahi-daemon || true

# Start avahi-daemon
echo "Starting avahi-daemon..."
avahi-daemon --no-rlimits --no-drop-root --daemonize

# Start pinstrel daemon in the background
echo "Starting pinstrel daemon..."
/usr/local/bin/pinstrel daemon --config /etc/pinstrel.toml &
pinstrel_PID=$!

# Wait briefly for socket setup
sleep 1

# Start shairport-sync in the background
echo "Starting shairport-sync..."
shairport-sync -c /etc/shairport-sync.conf &
SHAIRPORT_PID=$!

# Monitor processes
echo "Services started. Monitoring..."
while true; do
  if ! kill -0 $pinstrel_PID 2>/dev/null; then
    echo "ERROR: pinstrel daemon died."
    exit 1
  fi
  if ! kill -0 $SHAIRPORT_PID 2>/dev/null; then
    echo "ERROR: shairport-sync died."
    exit 1
  fi
  sleep 2
done
