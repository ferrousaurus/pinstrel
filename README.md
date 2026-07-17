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
make
```

This runs `go build -trimpath -ldflags '-s -w' -o pinstrel .` — stripping DWARF
and symbol tables. The strip is important on the Pi: the default Go build keeps
full debug info, and the link stage pre-allocates enough address space for it
that a Pi Zero 2 W (512 MB RAM, no swap by default) fails with:

```
…/link: mapping output file failed: cannot allocate memory
```

If `make` still fails with that error, you have two robust alternatives —
pick whichever is easier:

1. **Add swap on the Pi (canonical fix, ~1 minute):**

   ```bash
   sudo fallocate -l 2G /swapfile
   sudo chmod 600 /swapfile
   sudo mkswap /swapfile
   sudo swapon /swapfile
   echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
   make
   ```

   This gives the linker the slack it needs to `mmap` the output file. Swap is
   persistent across reboots via the `fstab` entry; remove it later with
   `sudo swapoff /swapfile && sudo rm /swapfile` and the corresponding
   `fstab` line if you don't want it permanent.

2. **Cross-compile from a more powerful machine** (your Mac, or any host with
   more than ~2 GB of free RAM). Use the provided Makefile targets:

   ```bash
   # arm64 Raspberry Pi (Pi 3 / 4 / 5 / Zero 2 W in 64-bit mode):
   make pi-arm64    # needs `gcc-aarch64-linux-gnu` (Debian: apt-get install gcc-aarch64-linux-gnu)

   # 32-bit armv7 Pi (Pi Zero W, original Pi):
   make pi-arm      # needs `gcc-arm-linux-gnueabihf` (Debian: apt-get install gcc-arm-linux-gnueabihf)
   ```

   Then copy the resulting `pinstrel` binary onto the Pi (e.g. `scp pinstrel pi:/tmp/`).

   macOS note: the `gcc-aarch64-linux-gnu`/`gcc-arm-linux-gnueabihf` cross
   toolchains aren't easy to install via Homebrew. The fastest path is to run
   the cross-build in a Linux container on the Mac, then `scp` the binary to
   the Pi:

   ```bash
   docker run --rm -v "$PWD:/src" -w /src golang:1.26 \
       sh -c 'apt-get update && apt-get install -y gcc-aarch64-linux-gnu && make pi-arm64'
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
# Seconds pinstrel waits for the full Discord voice handshake
# (VOICE_SERVER_UPDATE -> voice WS OP2 -> UDP IP-discovery -> Select Protocol)
# before abandoning the join and disconnecting cleanly. Discord's own internal
# wait is ~10s; 30s gives comfortable headroom for slow voice-server rotation.
VOICE_READY_TIMEOUT = 30
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

## How It Works Under the Hood

1. When you select **pinstrel AirPlay** from your iPhone/Mac audio output menu:
   - `shairport-sync` accepts the connection.
   - `shairport-sync` executes the script hook: `/usr/local/bin/pinstrel start`.
   - The CLI client sends a `start` command to `/tmp/pinstrel.sock`.
   - The `pinstrel` daemon pre-creates the audio FIFO if needed, kicks off the Discord voice join in the background, and returns `OK` to the hook immediately — so shairport never blocks on the (possibly slow) voice handshake.
   - In the background, the daemon concurrently waits for the voice WS/UDP handshake to finish (`VOICE_SERVER_UPDATE` → `OP2 READY` → UDP IP-discovery → `Select Protocol`) and for shairport to open the FIFO writer side. Once both succeed, it begins reading 48kHz PCM from the pipe.
   - The daemon encodes the PCM data into Opus frames and streams them to Discord.
2. When you stop AirPlay playback or disconnect:
   - `shairport-sync` executes the script hook: `/usr/local/bin/pinstrel stop` (triggered after the configured `session_timeout` of 10 seconds).
   - The CLI client sends a `stop` command to `/tmp/pinstrel.sock`.
   - The daemon closes the pipe, stops the streaming loop, and cleanly disconnects from the voice channel (sends Discord the nil-channel OP4 so no ghost presence remains).

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
- **`could not connect to pinstrel daemon ... no such file or directory` from shairport-sync**:
  Almost always means the pinstrel systemd unit has `PrivateTmp=true` enabled (or another form of `/tmp` isolation). A private `/tmp` makes both `/tmp/pinstrel.sock` and the audio FIFO `/tmp/shairport-sync-audio` invisible to `shairport-sync`, which runs in the host `/tmp` namespace. The shipped `pinstrel.service` deliberately omits `PrivateTmp`; do not re-add it. If you customized the unit, remove `PrivateTmp=true` and run `sudo systemctl daemon-reload && sudo systemctl restart pinstrel`.
- **Bot joins but stays deafened and `ERR: failed to join voice channel: timeout waiting for voice` appears in shairport-sync logs**:
  This is Discord's voice WS/UDP handshake failing to complete within discordgo's ~10s internal `waitUntilConnected` poll, surfaced to the shairport hook as an error. The full handshake is: gateway `VOICE_STATE_UPDATE` (discordgo gets the bot's `session_id`) -> `VOICE_SERVER_UPDATE` (discordgo gets the voice endpoint + token, opens the voice WS) -> voice WS `OP2` READY (discordgo gets SSRC + UDP port, opens the UDP socket, does IP discovery) -> `Select Protocol` (discordgo tells Discord the encryption mode). A stall at any stage yields `timeout waiting for voice`.

  Symptom patterns to look for in `sudo journalctl -u pinstrel -f` (with the daemon's discordgo `LogLevel = LogInformational`):
  - **`VOICE_STATE_UPDATE` never logged for the bot** → gateway never dispatched, or `s.State.User.ID` is unpopulated. Verify the bot token is correct and that the bot is in the server.
  - **`VOICE_SERVER_UPDATE` never logged** → the bot lacks `Connect`/`Speak`/`Use Voice Activity` permissions on the target channel (see Section 1 step 5), or the channel id is wrong.
  - **`connecting to udp addr ...` logged, then nothing** → UDP IP-discovery reply dropped (most often because discordgo v0.29.0 advertised the deprecated `xsalsa20_poly1305` Select Protocol mode that modern Discord voice servers reject). pinstrel now tracks discordgo `master`, which advertises `aead_aes256_gcm_rtpsize`; if you've pinned an older commit, re-run `go get github.com/bwmarrin/discordgo@master && go build && sudo cp pinstrel /usr/local/bin/`.
  - **`Voice join exceeded ... deadline — abandoning`** from pinstrel itself (not from discordgo) → the handshake stalled longer than `VOICE_READY_TIMEOUT` (default 30s). The bot is auto-disconnected (gateway nil-channel OP4) so no "ghost" lingers; raise the timeout only if you have evidence of legitimately slow voice-server rotation.

  Architecturally, the daemon returns `OK` to shairport's `run_this_before_play_begins` hook *before* the handshake completes, so a handshake failure no longer blocks shairport's own hook timeout — eliminating the old "drops and rejoins" retry loop. A failed join just logs and disconnects; start the next AirPlay session to retry.
- **Bot appears "deafened" in the channel UI even when audio is playing correctly**:
  Intentional. pinstrel is a play-only bot; it joins with `deaf=true` so Discord knows it won't receive audio. The "deafened" UI badge is the visual side-effect of that optimization and does not affect sending.
- **A leaked goroutine blocks on FIFO open after a handshake failure**:
  If the voice join fails before shairport opens the audio FIFO for writing, the background goroutine that opened the FIFO for reading is stuck on `os.OpenFile` (FIFO opens block until a writer connects). It is not cancelled automatically — there's no portable way to interrupt a blocking FIFO open in Go — and exits naturally when shairport next opens the writer side or when the daemon process exits. It holds only an FD slot, no Discord state. Acceptable tradeoff given how rare the path is in practice.
- **No AirPlay entry appears, but the bot joined Discord**:
  This means `shairport-sync`/`avahi-daemon` failed to advertise via mDNS while the Discord daemon (a separate TCP path to Discord's gateway) succeeded. Check `docker logs` for `couldn't create avahi client: Daemon not running!`, `Could not establish mDNS advertisement!`, or the entrypoint's `ERROR: UDP 5353 is already bound`. The usual cause is the host's `avahi-daemon` still holding UDP 5353 (often because `avahi-daemon.socket` is still active-listening after only `systemctl stop avahi-daemon` was run). See the [CAUTION](#4-running-with-docker) in Section 4 — apply `systemctl stop/disable/mask avahi-daemon avahi-daemon.socket` on the host, verify `sudo ss -lunp | grep :5353` prints nothing, then restart the container. Note: the container now refuses to start at all if 5353 is taken, so an explicit pre-flight error in `docker logs` confirms this is your problem.
