# pinstrel

`pinstrel` is a lightweight Go daemon and CLI utility designed to stream AirPlay audio directly to a Discord voice channel. It runs on a Raspberry Pi (optimized for the low-resource Raspberry Pi Zero 2 W) alongside `shairport-sync`.

## Features

- **Zero-Latency Stream Start/Stop**: Intercepts AirPlay session signals from `shairport-sync` using local IPC (Unix domain sockets).
- **Auto-Discovery**: Resolves the Discord Guild (server) ID automatically based on the configured Voice Channel ID.
- **Embedded Opus Encoding**: Uses CGO bindings to compile with standard `libopus`, performing high-performance, CPU-efficient audio encoding directly on the Pi.
- **Configurable Bitrate**: Streams high-fidelity audio up to the Discord channel limit (defaults to 128kbps).
- **Pure-Pipe Routing**: Resamples AirPlay audio (44.1kHz) to Discord voice standards (48kHz) inside `shairport-sync` to avoid running CPU-heavy subprocesses (like `ffmpeg`).

---

## 1. Discord Bot Setup

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications).
2. Create a **New Application** and add a **Bot** to it.
3. In the Bot settings, ensure that the **Server Members Intent** intent is enabled under **Privileged Gateway Intents**.
4. Generate a Bot Token and save it for configuration.
5. Generate an Invite Link for the bot using the URL Generator:
   - Select the `bot` scope.
   - Select the following **Bot Permissions**:
     - **General**: `View Channel`
     - **Voice**: `Connect`, `Speak`, `Use Voice Activity`
6. Invite the bot to your Discord server.
7. Copy the **ID of the voice channel** you want the bot to join (enable Discord Developer Mode to right-click and copy IDs).

---

## 2. Raspberry Pi Installation

### Step 2.1: System Dependencies

Install Go, build tools, and the Opus library headers:

```bash
sudo apt-get update
sudo apt-get install -y build-essential libopus-dev pkg-config git go
```

### Step 2.2: Install & Configure Shairport Sync

Shairport Sync must support the `pipe` audio backend. Install it via apt:

```bash
sudo apt-get install -y shairport-sync
```

_Note: If your system's package manager version of `shairport-sync` is outdated or lacks the pipe backend, you can compile it from source using `./configure --with-pipe --with-metadata --with-systemd`._

1. Copy the template configuration `shairport-sync.conf.template` from this repo to `/etc/shairport-sync.conf`:
   ```bash
   sudo cp shairport-sync.conf.template /etc/shairport-sync.conf
   ```
2. Restart `shairport-sync`:
   ```bash
   sudo systemctl restart shairport-sync
   ```

---

## 3. Build & Install pinstrel

### Step 3.1: Build the Binary

Clone this repository to your Raspberry Pi (or copy the files) and build:

```bash
go build -o pinstrel
```

### Step 3.2: Install the CLI and Daemon

Move the built binary to `/usr/local/bin` so it is globally available and accessible by `shairport-sync` hooks:

```bash
sudo cp pinstrel /usr/local/bin/
```

### Step 3.3: Configuration File

Create the configuration file at `/etc/pinstrel.toml`:

```toml
# /etc/pinstrel.toml

DISCORD_TOKEN = "YOUR_DISCORD_BOT_TOKEN"
DISCORD_CHANNEL_ID = "YOUR_DISCORD_VOICE_CHANNEL_ID"

# Optional settings (defaults shown below)
BITRATE = 128000
PIPE_PATH = "/tmp/shairport-sync-audio"
SOCKET_PATH = "/tmp/pinstrel.sock"
```

### Step 3.4: Install and Enable Systemd Service

Copy the systemd service file to the system config and enable it:

```bash
sudo cp pinstrel.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable pinstrel
sudo systemctl start pinstrel
```

---

## 4. Running with Docker

Alternatively, you can run both `pinstrel` and `shairport-sync` in a single Docker container. This is the recommended approach for clean installations.

### Step 4.1: Build the Image

Build the image locally:

```bash
docker build -t pinstrel:latest .
```

### Step 4.2: Run with Docker Compose

Using `docker-compose.yml` is the easiest way to configure and run the container:

```yaml
version: "3.8"

services:
  pinstrel:
    image: pinstrel:latest
    container_name: pinstrel
    network_mode: host
    restart: always
    environment:
      - DISCORD_TOKEN=YOUR_DISCORD_BOT_TOKEN
      - DISCORD_CHANNEL_ID=YOUR_DISCORD_VOICE_CHANNEL_ID
      - BITRATE=128000
```

Run the service:

```bash
docker compose up -d
```

### Step 4.3: Run with Docker CLI

If you prefer standard Docker commands, run:

```bash
docker run -d \
  --name pinstrel \
  --net=host \
  --restart=always \
  -e DISCORD_TOKEN="YOUR_DISCORD_BOT_TOKEN" \
  -e DISCORD_CHANNEL_ID="YOUR_DISCORD_VOICE_CHANNEL_ID" \
  -e BITRATE=128000 \
  pinstrel:latest
```

> [!IMPORTANT]
> **Network Mode Host**: You **must** use `--net=host` (`network_mode: host` in Docker Compose). This allows `avahi-daemon` inside the container to broadcast mDNS discovery packets to your local network, enabling your Apple devices to find the "pinstrel AirPlay" speaker.

---

## How It Works Under the Hood

1. When you select **pinstrel AirPlay** from your iPhone/Mac audio output menu:
   - `shairport-sync` accepts the connection.
   - `shairport-sync` executes the script hook: `/usr/local/bin/pinstrel start`.
   - The CLI client sends a `start` command to `/tmp/pinstrel.sock`.
   - The `pinstrel` daemon joins the voice channel, opens the named pipe `/tmp/shairport-sync-audio`, and begins reading the 48kHz PCM data.
   - The daemon encodes the PCM data into Opus frames and streams them to Discord.
2. When you stop AirPlay playback or disconnect:
   - `shairport-sync` executes the script hook: `/usr/local/bin/pinstrel stop` (triggered after the configured `session_timeout` of 10 seconds).
   - The CLI client sends a `stop` command to `/tmp/pinstrel.sock`.
   - The daemon closes the pipe, stops the streaming loop, and disconnects from the voice channel.

## Troubleshooting

- **Check logs for pinstrel daemon**:
  ```bash
  sudo journalctl -u pinstrel -f
  ```
- **Check logs for shairport-sync**:
  ```bash
  sudo journalctl -u shairport-sync -f
  ```
- **Test the pipe manually**:
  If the daemon is running but there is no sound, check if audio is actually being written to the pipe by running `cat /tmp/shairport-sync-audio` while AirPlay is active (it should output binary data in the terminal).
- **Check socket permissions**:
  Ensure `/tmp/pinstrel.sock` is writeable by the `shairport-sync` user. The daemon automatically runs `chmod 0777` on the socket file at startup.
