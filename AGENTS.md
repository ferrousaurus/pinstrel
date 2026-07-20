# AGENTS.md

Notes for OpenCode sessions working in this repo. Compact by design — only
what is non-obvious or easy to get wrong.

## What this is

`pinstrel` is a single-module Go daemon + CLI that streams AirPlay audio
(captured by `shairport-sync` into a FIFO) to a Discord voice channel, Opus-encoded via CGO. Target host is a Raspberry Pi Zero 2 W. Layout follows the
idiomatic Go `cmd/` + `internal/` pattern:

- `cmd/pinstrel/main.go` — subcommand dispatch: `daemon` (long-running), `start`/`stop` (one-shot IPC clients invoked by shairport-sync hooks).
- `internal/daemon/` — the stream lifecycle state machine; orchestrates voice join, pipe read, Opus send, and cleanup. Implements `ipc.CommandHandler`.
- `internal/discord/` — `discord.Session` / `discord.Connection` interfaces + a `*discordgo.Session` adapter, split across `client.go` (adapter core + `Session`), `voice.go` (voice-specific: `Connection`, `JoinChannel`, `ResolveUserVoiceState`, `AddDiagnostics`), and `slash.go` (slash command framework + builtins like `PingCommand`). The narrow `Session` interface makes `streamLoop` testable without a live Discord connection.
- `internal/ipc/` — Unix-socket server (`CommandHandler` interface) + `Send` one-shot client used by `start`/`stop`.
- `internal/audio/` — Opus encoder wrapper + pure `DecodePCMFrame` + frame constants.
- `internal/config/` — TOML loader (`config.go` / `config_test.go`).
- `deployments/systemd/pinstrel.service`, `configs/shairport-sync.conf.template` — deploy artifacts.

Audio path: shairport-sync → `/tmp/shairport-sync-audio` FIFO (48kHz S16LE stereo) → pinstrel Opus-encodes → Discord voice UDP. The daemon also listens on `/tmp/pinstrel.sock` for `start`/`stop` from shairport hooks.

## Build & verify

```bash
make                 # go build -trimpath -ldflags '-s -w' -> ./dist/pinstrel
make test            # go vet ./... && go test ./...
go test ./internal/config/...   # run a single package's tests
```

The Makefile's `-s -w` strip is load-bearing on the Pi: without it the linker
mmaps enough address space for DWARF that a Pi Zero 2 W (512 MB, no swap) fails
with `mapping output file failed: cannot allocate memory`. Don't switch to a
plain `go build` for Pi deploys. README §3.1 has the canonical swap workaround.

### CGO / system deps

`gopkg.in/hraban/opus.v2` is CGO against `libopus`. Building requires
`build-essential libopus-dev pkg-config` (Debian). `go build` will fail with
cgo/opus errors if the headers aren't installed — check this before debugging
anything else when builds break in a fresh environment.

### Cross-compile (from macOS/Linux for the Pi)

```bash
make pi-arm64   # needs gcc-aarch64-linux-gnu; on macOS use the docker recipe in README §3.1
make pi-arm     # 32-bit armv7, needs gcc-arm-linux-gnueabihf
```

Toolchain version pinned in `go.mod`: `go 1.26.5`.

## Gotchas that bite

- **There is no `--config` flag.** pinstrel is a system daemon wired to a
  fixed config path (`/etc/pinstrel.toml`, hard-coded in `cmd/pinstrel/main.go`).
  A missing file is a hard error — `LoadConfig` does not silently substitute
  defaults. For local dev, point your shell at a temp config by editing
  `configPath` in `cmd/pinstrel/main.go` or symlink `/etc/pinstrel.toml` to
  your working copy.
- **`config.toml` (in the repo root) is gitignored and not tracked.** The
  working copy here happens to contain a live Discord token — do not commit it,
  do not paste it into commits/PRs, and don't add a tracked sample that mirrors
  its real values. Config schema is documented in README §3.3 and `internal/config/config.go`.
- **Do not remove the `replace` directive in `go.mod`.** It pins a DAVE
  (Discord voice E2EE) fork of `github.com/bwmarrin/discordgo`
  (`yeongaori/discordgo …3d3293e4c765`, head of upstream PR #1704). Without it,
  Discord rejects the voice WS handshake with close code `4017` (enforced
  since March 1, 2026). Swap-back instructions are in README §3.5 — only do it
  when PR #1704 merges upstream. `go mod tidy` is safe; never let it delete
  the `replace` line while the pin is still needed.
- **Do not re-add `PrivateTmp=true` to `deployments/systemd/pinstrel.service`.** Both the IPC
  socket (`/tmp/pinstrel.sock`) and the audio FIFO live in `/tmp` and must be
  visible to `shairport-sync`, which runs in the host `/tmp` namespace. The
  service deliberately hardens everything *except* `/tmp` isolation.
- **Bot joining "deafened" is intentional.** pinstrel sets `deaf=true`
  (play-only bot); the UI badge is expected and does not affect sending.
- **Async start architecture.** The daemon resolves the configured user's
  current voice channel from the gateway `State` cache synchronously inside
  `HandleStart`, then returns `OK` to the shairport
  `run_this_before_play_begins` hook *before* the Discord voice WS/UDP
  handshake completes, bounded by `VOICE_READY_TIMEOUT` (default 30s). If the
  user is not in a voice channel (or shares no guild with the bot),
  `HandleStart` returns a sentinel error (`discord.ErrUserNotInVoice` /
  `discord.ErrUserSharesNoGuild`) *before* any side effects: no FIFO is
  created, no flags are set, no `streamLoop` is spawned. The IPC server
  surfaces this as `ERR: ...`, `ipc.Send` translates that into a Go error, and
  `pinstrel start` exits non-zero — so the shairport hook aborts the AirPlay
  play rather than proceeding to open a (rejected) FIFO writer. (Verify
  shairport-sync's documented behavior on non-zero hook exit before relying
  on the abort-the-play guarantee.) A single failed handshake (timeout or
  error from `JoinChannel`) produces one clean join+disconnect and the
  stream aborts; there is no partial-join fan-out to fall back on.
  Diagnose via `journalctl -u pinstrel -f` — look for `Successfully joined
  voice channel <id>`.
- **A goroutine opened on the FIFO for reading can block forever** if the
  voice join fails before shairport opens the writer side. Go has no portable
  way to interrupt a blocking FIFO `open`. This is a known accepted tradeoff
  (holds only an FD slot, no Discord state); do not "fix" it by adding
  cancellable openers without understanding why the comment in
  `internal/daemon/daemon.go`
  says it can't be done portably.

## CI

`.github/workflows/release-linux-arm64.yml` is **manual only**
(`workflow_dispatch`) and builds a `linux/arm64` release artifact on
`ubuntu-24.04-arm` (native ARM runner, not cross-compile). There is **no
PR/push CI** running tests or vet — run `make test` locally before pushing.
Releases are auto-tagged `vYYYYMMDD-<run_number>` unless a tag is supplied.

## Deploy recap (Pi)

`sudo cp dist/pinstrel /usr/local/bin/` → `sudo cp deployments/systemd/pinstrel.service /etc/systemd/system/` → `daemon-reload && enable && start pinstrel`. See README for the full flow; the service file's `ExecStart` is `pinstrel daemon` (no flags — config path is hard-coded to `/etc/pinstrel.toml`).