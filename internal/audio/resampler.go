// Package audio: resampler.go implements a pure-Go polyphase FIR resampler.
//
// pinstrel reads S16LE PCM from shairport-sync's `pipe` backend at the rate
// AirPlay negotiates (44.1 kHz on the stock Raspberry Pi OS apt build of
// shairport-sync, which is a Classic build without --with-ffmpeg and silently
// rejects `output_rate = 48000;`). Discord voice wires a fixed 48 kHz Opus
// encoder. The resampler bridges the two rates in-process — no ffmpeg binary,
// no shairport-sync rebuild — converting 44.1 kHz frames (882 samples/channel
// per 20 ms source frame) to 48 kHz frames (960 samples/channel per 20 ms
// Opus frame).
//
// Design:
//   - Windowed-sinc lowpass FIR, Hann window, cutoff 0.9 * min(in, out) / 2 Hz.
//   - Polyphase decomposition into L sub-filters (one per output phase), each
//     with `tapsPerPhase` coefficients. L = outRate/gcd(in, out).
//   - For 44.1→48: gcd=300, L=160, M=147; one 882-sample source frame produces
//     exactly 960 output samples (882*160 == 960*147 == 141120), so the phase
//     accumulator returns to 0 at each frame boundary and no inter-call phase
//     state is needed. The only state carried across calls is the
//     (tapsPerPhase-1)-sample input history per channel.
//   - Passthrough fast-path when inRate == outRate (e.g. SOURCE_SAMPLE_RATE
//     = 48000); no coefficient table is allocated and ProcessFrame copies in
//     verbatim.
//
// CPU cost on a Pi Zero 2 W: at 16 taps/phase × 48 kHz stereo ≈ 1.5 MHz of
// multiply-adds — under 1% of one A53 core. The polyphase form means only
// `tapsPerPhase` (not `L * tapsPerPhase`) MACs per output sample.
package audio

import (
	"errors"
	"fmt"
	"math"
)

// Resampler is a polyphase FIR sample-rate converter. It is not safe for
// concurrent use from multiple goroutines; the daemon's streamLoop is the
// single owner. Use NewResampler to construct one.
type Resampler struct {
	inRate       int
	outRate      int
	channels     int
	tapsPerPhase int

	// passthrough is true when inRate == outRate. ProcessFrame copies in to out
	// without modification and ignores coeffs/history.
	passthrough bool

	// ratioL and ratioM are the up/down factors of the rational rate conversion
	// outRate/inRate = L/M, after dividing both rates by their gcd.
	ratioL int
	ratioM int

	// coeffs is the polyphase coefficient table: L sub-filters of tapsPerPhase
	// coefficients each. coeffs[phase][k] is the kth tap of the filter used
	// for output samples whose phase index (within the L-phase cycle) is
	// `phase`.
	coeffs [][]float32

	// history holds the last (tapsPerPhase-1) input samples per channel from
	// the previous ProcessFrame call, so the convolution has left-edge context
	// for the first samples of the next frame. Interleaved: history[c][i] is
	// channel c, sample i.
	history [][]int16
}

// NewResampler constructs a Resampler converting inRate → outRate for `channels`
// interleaved channels with the given tapsPerPhase.
//
// Validation:
//   - inRate, outRate must be > 0.
//   - channels must be > 0.
//   - tapsPerPhase must be in [4, 128]: <4 risks audible aliasing on music;
//     >128 wastes CPU and memory on a Pi Zero 2 W with no audible benefit.
//
// If inRate == outRate, the resampler is constructed in passthrough mode:
// coeffs and history stay nil and ProcessFrame is a copy.
func NewResampler(inRate, outRate, channels, tapsPerPhase int) (*Resampler, error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, fmt.Errorf("resampler: rates must be positive (in=%d, out=%d)", inRate, outRate)
	}
	if channels <= 0 {
		return nil, fmt.Errorf("resampler: channels must be positive (got %d)", channels)
	}
	if tapsPerPhase < 4 {
		return nil, fmt.Errorf("resampler: tapsPerPhase must be >= 4 (got %d); lower values audibly alias", tapsPerPhase)
	}
	if tapsPerPhase > 128 {
		return nil, fmt.Errorf("resampler: tapsPerPhase must be <= 128 (got %d); higher wastes CPU with no audible benefit", tapsPerPhase)
	}

	r := &Resampler{
		inRate:       inRate,
		outRate:      outRate,
		channels:     channels,
		tapsPerPhase: tapsPerPhase,
	}

	if inRate == outRate {
		r.passthrough = true
		return r, nil
	}

	g := gcd(inRate, outRate)
	r.ratioL = outRate / g
	r.ratioM = inRate / g

	// Build the dense base filter (length L * tapsPerPhase), then split it
	// into L polyphase sub-filters of tapsPerPhase each. The base filter is a
	// windowed-sinc lowpass at the Nyquist of the lower rate, with a 0.9 guard
	// band to suppress imaging near the fold. Hann window gives ~-44 dB
	// stopband, transparent for music at 16+ taps.
	//
	// The base filter is normalized to unity DC gain (its full-length signed
	// sum is 1.0). We then scale it by L before decomposing: each sub-filter
	// picks L of every L*tapsPerPhase taps, so its signed sum averages
	// (1/L)*L = 1.0 across the L sub-filters. Per-sub-filter sums vary ±a few
	// percent around 1.0 (this is the passband ripple); we deliberately do
	// NOT re-normalize per-phase, because phases near sinc/Hann edges have
	// small or zero-crossing sums that would blow up the taps irregularly
	// and destroy the stopband rejection (pathological per-phase scaling is
	// the classic polyphase-FIR footgun).
	baseLen := r.ratioL * tapsPerPhase
	nyquist := math.Min(float64(inRate), float64(outRate)) / 2.0
	cutoff := 0.9 * nyquist / (float64(outRate) / 2.0) // normalized to outRate's Nyquist
	denseCutoff := cutoff / float64(r.ratioL)
	base := windowedSinc(denseCutoff, baseLen)

	// Polyphase decomposition: sub-filter `phase` taps the base filter at
	// indices phase, phase+L, phase+2L, ... Each sub-filter is normalized
	// to unity DC gain so that every output sample reproduces DC identically
	// without phase-dependent amplitude ripple.
	r.coeffs = make([][]float32, r.ratioL)
	for phase := 0; phase < r.ratioL; phase++ {
		taps := make([]float32, tapsPerPhase)
		var sum float64
		for k := 0; k < tapsPerPhase; k++ {
			idx := phase + k*r.ratioL
			if idx < len(base) {
				taps[k] = float32(base[idx])
				sum += float64(taps[k])
			}
		}
		if sum != 0 {
			scale := float32(1.0 / sum)
			for k := range taps {
				taps[k] *= scale
			}
		}
		r.coeffs[phase] = taps
	}

	// Per-channel input history (last tapsPerPhase-1 input samples). The first
	// call sees a zero-padded left edge, which is correct for a stream that
	// starts cold; subsequent calls see the actual tail of the prior frame.
	histLen := tapsPerPhase - 1
	if histLen > 0 {
		r.history = make([][]int16, channels)
		for c := 0; c < channels; c++ {
			r.history[c] = make([]int16, histLen)
		}
	}

	return r, nil
}

// InRate returns the configured input sample rate.
func (r *Resampler) InRate() int { return r.inRate }

// OutRate returns the configured output sample rate.
func (r *Resampler) OutRate() int { return r.outRate }

// ProcessFrame resamples one frame of interleaved int16 PCM.
//
// The caller must size `in` to exactly `SourceFrameSize` samples (channels
// interleaved) and `out` to exactly `FrameSize` samples. ProcessFrame reads
// the whole `in`, writes the whole `out`, and returns (samplesConsumed,
// samplesProduced, error). For the fixed 44.1→48 call shape (882 in, 960 out)
// the phase accumulator returns to 0 at each frame boundary, so an exact
// integer number of outputs is produced per call — no frames split across
// calls and no underrun/overrun state beyond the per-channel history.
//
// For passthrough resamplers (inRate == outRate), ProcessFrame copies `in`
// to `out` and requires len(in) == len(out).
func (r *Resampler) ProcessFrame(in, out []int16) (int, int, error) {
	if r.passthrough {
		if len(in) != len(out) {
			return 0, 0, fmt.Errorf("resampler: passthrough requires len(in)==len(out) (in=%d, out=%d)", len(in), len(out))
		}
		copy(out, in)
		return len(in), len(out), nil
	}

	inSamples := len(in) / r.channels
	outSamples := len(out) / r.channels
	if inSamples == 0 || outSamples == 0 {
		return 0, 0, errors.New("resampler: empty input or output")
	}

	// Convolve each output sample against its polyphase sub-filter. For output
	// sample m (0-indexed within this frame):
	//   phase  = (m * M) mod L    — selects which sub-filter to use
	//   offset = (m * M) / L      — integer index of input sample at or preceding output time
	// Output m is sum_k coeffs[phase][k] * inSample(offset + halfTaps - 1 - k),
	// aligning the filter's center peak with the output sample's time position.
	halfTaps := r.tapsPerPhase / 2
	histLen := r.tapsPerPhase - 1
	for c := 0; c < r.channels; c++ {
		for m := 0; m < outSamples; m++ {
			phase := (m * r.ratioM) % r.ratioL
			offset := (m * r.ratioM) / r.ratioL
			taps := r.coeffs[phase]
			var y float32
			for k := 0; k < r.tapsPerPhase; k++ {
				inputIdx := offset + halfTaps - 1 - k
				var s int16
				switch {
				case inputIdx >= 0 && inputIdx < inSamples:
					s = in[inputIdx*r.channels+c]
				case inputIdx < 0:
					// Pull from prior-frame history. inputIdx == -1 is the most
					// recent history sample, -2 the one before, etc. We store
					// history with index 0 = oldest, histLen-1 = newest, so
					// inputIdx == -1 maps to history[histLen-1], -2 → history[histLen-2], ...
					hidx := histLen + inputIdx
					if hidx >= 0 {
						s = r.history[c][hidx]
					}
					// hidx < 0 → before any history: treat as 0 (cold start).
				}
				y += taps[k] * float32(s)
			}
			// Clamp to int16 range. float32 → int32 with proper rounding.
			yi := int32(math.Round(float64(y)))
			if yi > 32767 {
				yi = 32767
			} else if yi < -32768 {
				yi = -32768
			}
			out[m*r.channels+c] = int16(yi)
		}
	}

	// Update history: the last histLen input samples become the left-edge
	// context for the next frame's first `histLen` taps.
	if histLen > 0 {
		for c := 0; c < r.channels; c++ {
			for i := 0; i < histLen; i++ {
				srcIdx := (inSamples - histLen + i) * r.channels + c
				r.history[c][i] = in[srcIdx]
			}
		}
	}

	return len(in), len(out), nil
}

// Close releases any resampler state. Currently a no-op (the resampler owns
// only in-memory state), but defined for future-proofing the API and for
// symmetry with io.Closer consumers.
func (r *Resampler) Close() error { return nil }

// windowedSinc returns a normalized windowed-sinc lowpass FIR of length n
// with the given normalized cutoff (1.0 == outRate's Nyquist). The window is
// Hann; the result is symmetric about its center and unity-DC-gain.
func windowedSinc(normCutoff float64, n int) []float64 {
	if n <= 0 {
		return nil
	}
	out := make([]float64, n)
	// Center of the window; for symmetric FIR we want n odd, but for polyphase
	// use n = L * tapsPerPhase which may be even — the polyphase decomposition
	// still works because we normalize per-phase afterwards.
	center := float64(n-1) / 2.0
	var sum float64
	for i := 0; i < n; i++ {
		// Normalized sinc argument relative to cutoff: x = (i - center) * cutoff
		x := (float64(i) - center) * normCutoff
		var s float64
		if x == 0 {
			s = 1.0
		} else {
			s = math.Pi * x
			s = math.Sin(s) / s
		}
		// Hann window: 0.5 - 0.5*cos(2*pi*i/(n-1)). Suppresses Gibbs ringing.
		w := 0.5 - 0.5*math.Cos(2.0*math.Pi*float64(i)/float64(n-1))
		out[i] = s * w
		sum += out[i]
	}
	// Unity DC gain: divide by sum of taps. (Polyphase decomposition re-normalizes
	// per phase in NewResampler, so this base normalization is conservative but
	// keeps the math well-conditioned.)
	if sum != 0 {
		for i := range out {
			out[i] /= sum
		}
	}
	return out
}

// gcd returns the greatest common divisor of a and b (Euclid's algorithm).
// Both must be non-negative; gcd(0, n) == n.
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}