# AGENTS.md

Notes for OpenCode sessions working in this repo. Compact by design — only
what is non-obvious or easy to get wrong.

## What this is

`pinstrel` is a single-module Go daemon + CLI that streams AirPlay audio
(captured by `shairport-sync` into a FIFO) to a Discord voice channel, Opus-encoded via CGO. Target host is a Raspberry Pi Zero 2 W. Flat layout:
everything is `package main` in the repo root.

- `main.go` — subcommand dispatch: `daemon` (long-running), `start`/`stop` (one-shot IPC clients invoked by shairport-sync hooks).
- `daemon.go` — Discord session, Unix-socket IPC server, Opus encoder, FIFO reader. ~500 lines, the bulk of the logic.
- `cli.go` — `SendIPCCommand` over `net.Dial("unix", …)`.
- `config.go` / `config_test.go` — TOML loader, plus the only tests in the repo.
- `Makefile`, `pinstrel.service`, `shairport-sync.conf.template` — deploy artifacts.

Audio path: shairport-sync → `/tmp/shairport-sync-audio` FIFO (48kHz S16LE stereo) → pinstrel Opus-encodes → Discord voice UDP. The daemon also listens on `/tmp/pinstrel.sock` for `start`/`stop` from shairport hooks.

## Build & verify

```bash
make                 # go build -trimpath -ldflags '-s -w' -> ./dist/pinstrel (NOT ./pinstrel)
make test            # go vet ./... && go test ./...
go test -run TestLoadConfig ./...   # run a single test (only config_test.go exists)
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
  fixed config path (`/etc/pinstrel.toml`, hard-coded in `main.go`). A missing
  file is a hard error — `LoadConfig` does not silently substitute defaults.
  For local dev, point your shell at a temp config by editing `configPath` in
  `main.go` or symlink `/etc/pinstrel.toml` to your working copy.
- **`config.toml` (in the repo root) is gitignored and not tracked.** The
  working copy here happens to contain a live Discord token — do not commit it,
  do not paste it into commits/PRs, and don't add a tracked sample that mirrors
  its real values. Config schema is documented in README §3.3 and `config.go`.
- **Do not remove the `replace` directive in `go.mod`.** It pins a DAVE
  (Discord voice E2EE) fork of `github.com/bwmarrin/discordgo`
  (`yeongaori/discordgo …3d3293e4c765`, head of upstream PR #1704). Without it,
  Discord rejects the voice WS handshake with close code `4017` (enforced
  since March 1, 2026). Swap-back instructions are in README §3.5 — only do it
  when PR #1704 merges upstream. `go mod tidy` is safe; never let it delete
  the `replace` line while the pin is still needed.
- **Do not re-add `PrivateTmp=true` to `pinstrel.service`.** Both the IPC
  socket (`/tmp/pinstrel.sock`) and the audio FIFO live in `/tmp` and must be
  visible to `shairport-sync`, which runs in the host `/tmp` namespace. The
  service deliberately hardens everything *except* `/tmp` isolation.
- **Bot joining "deafened" is intentional.** pinstrel sets `deaf=true`
  (play-only bot); the UI badge is expected and does not affect sending.
- **Async start architecture.** The daemon returns `OK` to the shairport
  `run_this_before_play_begins` hook *before* the Discord voice WS/UDP
  handshake completes, bounded by `VOICE_READY_TIMEOUT` (default 30s). A
  handshake failure produces one clean join+disconnect per AirPlay attempt,
  not the old retry loop. Diagnose via `journalctl -u pinstrel -f`.
- **A goroutine opened on the FIFO for reading can block forever** if the
  voice join fails before shairport opens the writer side. Go has no portable
  way to interrupt a blocking FIFO `open`. This is a known accepted tradeoff
  (holds only an FD slot, no Discord state); do not "fix" it by adding
  cancellable openers without understanding why the comment in `daemon.go`
  says it can't be done portably.

## CI

`.github/workflows/release-linux-arm64.yml` is **manual only**
(`workflow_dispatch`) and builds a `linux/arm64` release artifact on
`ubuntu-24.04-arm` (native ARM runner, not cross-compile). There is **no
PR/push CI** running tests or vet — run `make test` locally before pushing.
Releases are auto-tagged `vYYYYMMDD-<run_number>` unless a tag is supplied.

## Deploy recap (Pi)

`sudo cp dist/pinstrel /usr/local/bin/` → `sudo cp pinstrel.service /etc/systemd/system/` → `daemon-reload && enable && start pinstrel`. See README for the full flow; the service file's `ExecStart` already points at `/etc/pinstrel.toml`.