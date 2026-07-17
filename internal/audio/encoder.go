// Package audio wraps the Opus encoder for Discord voice's 48kHz stereo
// format and provides pure helpers for PCM frame conversion.
package audio

import (
	"encoding/binary"
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

const (
	// SampleRate is the PCM sample rate Discord voice expects (48kHz).
	// shairport-sync is configured to resample AirPlay's 44.1kHz up to this
	// so pinstrel never resamples.
	SampleRate = 48000
	// NumChannels is the channel count (stereo) for both PCM input and the
	// Opus encoder output.
	NumChannels = 2
	// MaxPacketSize is the output buffer for a single Opus frame. The
	// RFC 7587 max is 1275 bytes; 1024 covers a 128kbps frame with margin.
	MaxPacketSize = 1024

	// PCM frame layout for a 20ms Opus frame at 48kHz stereo S16LE:
	//   48000 Hz * 0.02s = 960 samples/channel
	//   960 * 2 channels = 1920 total samples
	//   1920 * 2 bytes/sample = 3840 bytes
	FrameSamples = SampleRate / 1000 * 20 // 960 samples per channel per 20ms frame
	FrameSize    = FrameSamples * NumChannels
	FrameBytes   = FrameSize * 2 // 16-bit samples
)

// Encoder wraps an Opus encoder configured for Discord voice output.
type Encoder struct {
	enc *opus.Encoder
}

// NewEncoder creates an Opus encoder at the given bitrate. The encoder is
// constructed for 48kHz stereo (Discord voice's fixed format); the bitrate is
// the only tunable parameter.
func NewEncoder(bitrate int) (*Encoder, error) {
	e, err := opus.NewEncoder(SampleRate, NumChannels, opus.AppAudio)
	if err != nil {
		return nil, fmt.Errorf("create opus encoder: %w", err)
	}
	return &Encoder{enc: e}, nil
}

// SetBitrate sets the encoder's target bitrate in bits per second.
func (e *Encoder) SetBitrate(bitrate int) error {
	return e.enc.SetBitrate(bitrate)
}

// Encode encodes a single PCM frame (FrameSize int16 samples) into dst and
// returns the number of bytes written.
func (e *Encoder) Encode(pcm []int16, dst []byte) (int, error) {
	return e.enc.Encode(pcm, dst)
}

// DecodePCMFrame converts a little-endian S16LE byte buffer into int16
// samples. It is the pure inverse of the wire format shairport-sync emits on
// its pipe backend. src must be exactly 2*len(dst) bytes.
func DecodePCMFrame(src []byte, dst []int16) {
	for i := range dst {
		dst[i] = int16(binary.LittleEndian.Uint16(src[i*2 : i*2+2]))
	}
}
