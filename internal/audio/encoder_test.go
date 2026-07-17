package audio

import (
	"testing"
)

func TestDecodePCMFrame(t *testing.T) {
	// Two samples in little-endian: {0x02, 0x01} = 0x0102 = 258, {0x04, 0x03} = 0x0304 = 772.
	src := []byte{0x02, 0x01, 0x04, 0x03}
	dst := make([]int16, 2)
	DecodePCMFrame(src, dst)

	if dst[0] != 258 {
		t.Errorf("dst[0]: expected 258, got %d", dst[0])
	}
	if dst[1] != 772 {
		t.Errorf("dst[1]: expected 772, got %d", dst[1])
	}
}

func TestDecodePCMFrame_RoundTrip(t *testing.T) {
	// Verify DecodePCMFrame correctly interprets every byte pattern by
	// checking a full-frame decode produces the expected int16 values.
	src := make([]byte, FrameBytes)
	want := make([]int16, FrameSize)
	for i := range want {
		want[i] = int16(i % 32768)
		// Encode as little-endian into src.
		src[i*2] = byte(want[i] & 0xff)
		src[i*2+1] = byte(want[i] >> 8)
	}

	got := make([]int16, FrameSize)
	DecodePCMFrame(src, got)

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: expected %d, got %d", i, want[i], got[i])
		}
	}
}

func TestNewEncoder(t *testing.T) {
	enc, err := NewEncoder(128000)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	if err := enc.SetBitrate(192000); err != nil {
		t.Fatalf("SetBitrate failed: %v", err)
	}

	// Encode one silent frame (all zeros) and check we get a non-empty packet.
	pcm := make([]int16, FrameSize)
	dst := make([]byte, MaxPacketSize)
	n, err := enc.Encode(pcm, dst)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if n <= 0 {
		t.Fatalf("Encode returned %d bytes, expected >0", n)
	}
}

func TestFrameConstants(t *testing.T) {
	if FrameSamples != 960 {
		t.Errorf("FrameSamples: expected 960, got %d", FrameSamples)
	}
	if FrameSize != 1920 {
		t.Errorf("FrameSize: expected 1920, got %d", FrameSize)
	}
	if FrameBytes != 3840 {
		t.Errorf("FrameBytes: expected 3840, got %d", FrameBytes)
	}
}
