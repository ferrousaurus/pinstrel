# pinstrel Makefile
#
# Default target: native build on the Pi. Strips DWARF/symbol tables and
# trims the local path prefix from the binary. The `-s -w` pair slashes
# link-time RSS substantially (CGO pulls libopus, so the default DWARF
# footprint is what trips the Pi Zero's 512MB-no-swap `mmap` failure
# `mapping output file failed: cannot allocate memory`).
#
# For more robust fixes see README "Step 3.1" — adding 2G of swap on the Pi
# is the canonical solution; cross-compiling from macOS or another host with
# more RAM (see `make pi-arm64` / `make pi-arm`) avoids the problem entirely.

LDFLAGS    := -s -w
GOFLAGS    := -trimpath
BINARY     := pinstrel

.PHONY: all clean pi-arm64 pi-arm build-native test

all: $(BINARY)

$(BINARY):
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o ./dist/$(BINARY) .

# Build on the Pi itself (same as `make all` — alias for clarity).
build-native: $(BINARY)

# Cross-compile from macOS / Linux for an arm64 Raspberry Pi (Pi 3/4/5/Zero 2 W
# in 64-bit mode). Requires the aarch64 cross toolchain for CGO (libopus).
#   macOS:  brew install FiloSottile/musl-cross/musl-cross       (armhf)
#          or use a Docker-based cross toolchain for aarch64.
#   Debian: sudo apt-get install gcc-aarch64-linux-gnu
pi-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 \
	CC=aarch64-linux-gnu-gcc \
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o ./dist/$(BINARY) .

# Cross-compile for a 32-bit armv6/7 Raspberry Pi (Pi Zero W, original Pi).
#   Debian: sudo apt-get install gcc-arm-linux-gnueabihf
pi-arm:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=1 \
	CC=arm-linux-gnueabihf-gcc \
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o ./dist/$(BINARY) .

test:
	go vet ./...
	go test ./...

clean:
	rm -f ./dist/$(BINARY)
