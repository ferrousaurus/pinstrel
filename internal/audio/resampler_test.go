package audio

import (
	"math"
	"testing"
)

// TestResampler_Ratio verifies that the canonical 44.1→48 frame conversion
// produces exactly the expected sample count: 882 source samples/channel →
// 960 output samples/channel. Zeros in → zeros out, so this is purely a
// contract test on the ratio and the frame-stability invariant
// (882*160 == 960*147 == 141120).
func TestResampler_Ratio(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, NumChannels, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}
	if r.passthrough {
		t.Fatal("44.1→48 resampler must not be passthrough")
	}
	if r.ratioL != 160 || r.ratioM != 147 {
		t.Errorf("ratio: expected L=160, M=147, got L=%d, M=%d", r.ratioL, r.ratioM)
	}

	in := make([]int16, SourceFrameSize)
	out := make([]int16, FrameSize)
	inUsed, outProduced, err := r.ProcessFrame(in, out)
	if err != nil {
		t.Fatalf("ProcessFrame: %v", err)
	}
	if inUsed != len(in) {
		t.Errorf("inUsed: expected %d, got %d", len(in), inUsed)
	}
	if outProduced != len(out) {
		t.Errorf("outProduced: expected %d, got %d", len(out), outProduced)
	}
	for i, v := range out {
		if v != 0 {
			t.Errorf("out[%d]: expected 0 (zeros in → zeros out), got %d", i, v)
		}
	}
}

// TestResampler_Passthrough verifies that inRate == outRate produces a
// verbatim-copy resampler with no coefficient table.
func TestResampler_Passthrough(t *testing.T) {
	r, err := NewResampler(SampleRate, SampleRate, NumChannels, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}
	if !r.passthrough {
		t.Fatal("equal-rate resampler must be passthrough")
	}
	if r.coeffs != nil {
		t.Error("passthrough resampler should not allocate coeffs")
	}
	if r.history != nil {
		t.Error("passthrough resampler should not allocate history")
	}

	// Identical in/out sizes are required for passthrough (caller passes the
	// same-sized buffer either way; the daemon's streamLoop uses FrameSize for
	// both srcPcmBuf and outPcmBuf when rates match... but ProcessFrame only
	// requires len(in) == len(out) for passthrough).
	in := make([]int16, FrameSize)
	for i := range in {
		in[i] = int16(i)
	}
	out := make([]int16, FrameSize)
	if _, _, err := r.ProcessFrame(in, out); err != nil {
		t.Fatalf("ProcessFrame passthrough: %v", err)
	}
	for i, v := range out {
		if v != in[i] {
			t.Errorf("passthrough out[%d]: expected %d, got %d", i, in[i], v)
		}
	}

	// Mismatched sizes must error.
	bad := make([]int16, FrameSize-1)
	if _, _, err := r.ProcessFrame(in, bad); err == nil {
		t.Error("passthrough with mismatched sizes should error")
	}
}

// TestResampler_ImpulseResponse feeds a single-sample impulse at the center
// of a source frame, then collects several frames of output. The polyphase
// filter's windowed-sinc envelope should propagate into the output across
// roughly L*tapsPerPhase/sourceRate ms (~9 ms for 16 taps at 44.1 kHz × L=160),
// and decay to numerical noise after the impulse has scrolled past. This is
// the standard polyphase FIR sanity check.
func TestResampler_ImpulseResponse(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, 1, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}

	// Place the impulse at the center of the input frame so the Hann-windowed
	// sinc has non-zero taps weighting it (putting the impulse at index 0
	// would hit window edge taps = 0, producing all-zero output by design).
	in := make([]int16, SourceFrameSamples)
	in[SourceFrameSamples/2] = 32767
	out := make([]int16, FrameSamples)

	// First frame: excites the filter. Should produce non-zero output.
	if _, _, err := r.ProcessFrame(in, out); err != nil {
		t.Fatalf("ProcessFrame: %v", err)
	}
	var maxAbs int16
	for _, v := range out {
		a := int16(absInt(int(v)))
		if a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		t.Fatal("impulse response: expected non-zero output, got all zeros")
	}

	// Drain a few more frames of zeros. The impulse scrolls past the filter
	// tail; output decays to numerical noise. 5 frames of 882 samples each =
	// 4410 samples = ~100 ms of source audio — far longer than the
	// (~16 taps * 160 phase / 44100 Hz ≈ 58 ms) impulse response extent.
	for i := 0; i < 5; i++ {
		zeroIn := make([]int16, SourceFrameSamples)
		if _, _, err := r.ProcessFrame(zeroIn, out); err != nil {
			t.Fatalf("ProcessFrame follow-up %d: %v", i, err)
		}
	}
	var maxTail int16
	for _, v := range out {
		a := int16(absInt(int(v)))
		if a > maxTail {
			maxTail = a
		}
	}
	if maxTail > 20 {
		t.Errorf("impulse tail after 6 frames: expected <20 (numerical noise), got max abs %d", maxTail)
	}
}

// TestResampler_Passband verifies that a 1 kHz sine at 44.1 kHz retains
// amplitude at 1 kHz at 48 kHz within ±3 dB after resampling. The Goertzel
// algorithm measures the resp at an exact frequency (no DFT bin-rounding),
// making the test robust to the off-bin case without needing windowing.
func TestResampler_Passband(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, 1, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}

	// 10 source frames = 8820 samples at 44.1 kHz ≈ 200 ms of 1 kHz sine.
	// 1 kHz at 44.1 kHz = 44.1 samples/cycle (non-integer, but Goertzel handles
	// it); 1 kHz at 48 kHz = 48 samples/cycle (integer — zero leakage on output).
	const numFrames = 10
	const inAmp = 30000.0
	const testFreq = 1000.0
	in := make([]int16, SourceFrameSamples)
	out := make([]int16, FrameSamples)
	allOut := make([]int16, 0, numFrames*FrameSamples)
	for i := 0; i < numFrames; i++ {
		for j := 0; j < SourceFrameSamples; j++ {
			tIdx := i*SourceFrameSamples + j
			in[j] = int16(inAmp * math.Sin(2*math.Pi*testFreq*float64(tIdx)/float64(SourceSampleRate)))
		}
		if _, _, err := r.ProcessFrame(in, out); err != nil {
			t.Fatalf("ProcessFrame %d: %v", i, err)
		}
		allOut = append(allOut, out...)
	}

	// Discard the first frame (startup transient from zero history), then run
	// a Goertzel measurement at the exact target frequency.
	skip := FrameSamples
	steady := allOut[skip:]
	outAmp := goertzelAmp(steady, SampleRate, testFreq)

	// Per-phase DC-gain normalization keeps passband roughly unity, with some
	// ripple from the sinc shape. Accept ±3 dB: this is a correctness check
	// (resampler preserved the tone at essentially its original amplitude),
	// not a precise level-match.
	ratio := outAmp / inAmp
	dB := 20 * math.Log10(ratio)
	if dB < -3.0 || dB > 3.0 {
		t.Errorf("passband: 1 kHz tone amplitude %g vs input %g (ratio %.2f, %.2f dB; expected within ±3 dB)", outAmp, inAmp, ratio, dB)
	}
}

// TestResampler_Stopband verifies that a 22 kHz tone (above the resampler's
// ~19.85 kHz lowpass cutoff at 0.9 * 22050, and just below source Nyquist) is
// attenuated by at least 25 dB after resampling. This catches insufficient
// stopband attenuation (which would manifest as audible aliasing on real
// audio played through the pipeline).
func TestResampler_Stopband(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, 1, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}

	// 22000 Hz at 44.1 kHz gives 44100/22000 = 2.0045 samples/cycle; over
	// 8820 samples that's 4400 cycles — exactly integer, so no end-effects
	// leakage in the Goertzel measurement. 22 kHz is below source Nyquist
	// (22050) and well past the cutoff (~19.85 kHz), squarely in the
	// stopband of a Hann-windowed 16-tap polyphase FIR.
	const numFrames = 10
	const inAmp = 30000.0
	const testFreq = 22000.0
	in := make([]int16, SourceFrameSamples)
	out := make([]int16, FrameSamples)
	allOut := make([]int16, 0, numFrames*FrameSamples)
	for i := 0; i < numFrames; i++ {
		for j := 0; j < SourceFrameSamples; j++ {
			tIdx := i*SourceFrameSamples + j
			in[j] = int16(inAmp * math.Sin(2*math.Pi*testFreq*float64(tIdx)/float64(SourceSampleRate)))
		}
		if _, _, err := r.ProcessFrame(in, out); err != nil {
			t.Fatalf("ProcessFrame: %v", err)
		}
		allOut = append(allOut, out...)
	}

	skip := FrameSamples
	steady := allOut[skip:]
	outAmp := goertzelAmp(steady, SampleRate, testFreq)

	// Stopband should be at least -25 dB at 22 kHz. The Hann window specifies
	// -44 dB stopband deeper in; the 16-tap polyphase's transition region is
	// wide, but 22 kHz is far enough past the 19.85 kHz cutoff to reach the
	// deep stopband.
	ratio := outAmp / inAmp
	dB := 20 * math.Log10(ratio)
	if dB > -25.0 {
		t.Errorf("stopband: 22 kHz tone amplitude %g vs input %g (ratio %.2f, %.2f dB; expected < -25 dB)", outAmp, inAmp, ratio, dB)
	}
}

// TestResampler_FrameStability runs 100 calls and verifies each produces
// exactly FrameSize output samples — no drift across frames.
func TestResampler_FrameStability(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, NumChannels, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}
	in := make([]int16, SourceFrameSize)
	out := make([]int16, FrameSize)
	for i := 0; i < 100; i++ {
		_, outProduced, err := r.ProcessFrame(in, out)
		if err != nil {
			t.Fatalf("ProcessFrame %d: %v", i, err)
		}
		if outProduced != FrameSize {
			t.Fatalf("frame %d: outProduced %d, expected %d (drift)", i, outProduced, FrameSize)
		}
	}
}

// TestResampler_InvalidArgs covers the validation guards in NewResampler.
func TestResampler_InvalidArgs(t *testing.T) {
	cases := []struct {
		name                                                          string
		inRate, outRate, channels, tapsPerPhase                       int
	}{
		{"zero inRate", 0, 48000, 2, 16},
		{"negative inRate", -44100, 48000, 2, 16},
		{"zero outRate", 44100, 0, 2, 16},
		{"zero channels", 44100, 48000, 0, 16},
		{"taps too low", 44100, 48000, 2, 3},
		{"taps too high", 44100, 48000, 2, 129},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewResampler(c.inRate, c.outRate, c.channels, c.tapsPerPhase); err == nil {
				t.Errorf("NewResampler(%d, %d, %d, %d) should have errored",
					c.inRate, c.outRate, c.channels, c.tapsPerPhase)
			}
		})
	}
}

// TestResampler_RateAccessors verifies InRate/OutRate report configured rates.
func TestResampler_RateAccessors(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, NumChannels, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}
	if r.InRate() != SourceSampleRate {
		t.Errorf("InRate: expected %d, got %d", SourceSampleRate, r.InRate())
	}
	if r.OutRate() != SampleRate {
		t.Errorf("OutRate: expected %d, got %d", SampleRate, r.OutRate())
	}
}

// TestResampler_PitchShift verifies that a 1000 Hz tone at 44.1 kHz remains
// 1000 Hz at 48 kHz (no pitch shift).
func TestResampler_PitchShift(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, 1, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}

	const numFrames = 50
	const inAmp = 30000.0
	const testFreq = 1000.0
	in := make([]int16, SourceFrameSamples)
	out := make([]int16, FrameSamples)
	allOut := make([]int16, 0, numFrames*FrameSamples)
	for i := 0; i < numFrames; i++ {
		for j := 0; j < SourceFrameSamples; j++ {
			tIdx := i*SourceFrameSamples + j
			in[j] = int16(inAmp * math.Sin(2*math.Pi*testFreq*float64(tIdx)/float64(SourceSampleRate)))
		}
		if _, _, err := r.ProcessFrame(in, out); err != nil {
			t.Fatalf("ProcessFrame: %v", err)
		}
		allOut = append(allOut, out...)
	}

	steady := allOut[FrameSamples*5:] // skip initial frames

	// Test amplitude at 1000 Hz vs 1088.4 Hz (pitch up) vs 918.75 Hz (pitch down)
	amp1000 := goertzelAmp(steady, SampleRate, 1000.0)
	amp1088 := goertzelAmp(steady, SampleRate, 1000.0 * 48000.0 / 44100.0)
	amp918 := goertzelAmp(steady, SampleRate, 1000.0 * 44100.0 / 48000.0)

	t.Logf("Amp at 1000 Hz: %.2f", amp1000)
	t.Logf("Amp at 1088.4 Hz (shifted up): %.2f", amp1088)
	t.Logf("Amp at 918.75 Hz (shifted down): %.2f", amp918)

	if amp1000 < 20000 || amp1088 > 5000 || amp918 > 5000 {
		t.Errorf("Pitch shift detected! 1000Hz amp=%.1f, 1088Hz amp=%.1f, 918Hz amp=%.1f", amp1000, amp1088, amp918)
	}
}

// TestResampler_SpectralPurity measures total harmonic distortion / noise
// of a resampled sine wave compared to a pure reference sine.
func TestResampler_SpectralPurity(t *testing.T) {
	r, err := NewResampler(SourceSampleRate, SampleRate, 1, DefaultTapsPerPhase)
	if err != nil {
		t.Fatalf("NewResampler: %v", err)
	}

	const numFrames = 50
	const inAmp = 30000.0
	const testFreq = 1000.0
	in := make([]int16, SourceFrameSamples)
	out := make([]int16, FrameSamples)
	allOut := make([]int16, 0, numFrames*FrameSamples)
	for i := 0; i < numFrames; i++ {
		for j := 0; j < SourceFrameSamples; j++ {
			tIdx := i*SourceFrameSamples + j
			in[j] = int16(inAmp * math.Sin(2*math.Pi*testFreq*float64(tIdx)/float64(SourceSampleRate)))
		}
		if _, _, err := r.ProcessFrame(in, out); err != nil {
			t.Fatalf("ProcessFrame: %v", err)
		}
		allOut = append(allOut, out...)
	}

	// Skip startup frames
	startSample := FrameSamples * 5
	steady := allOut[startSample:]

	// Fit ideal sine: amplitude ~ inAmp, freq = testFreq (1000 Hz at 48000 Hz)
	var maxErr float64
	var sumErrSq float64
	var sumRefSq float64
	for m, val := range steady {
		globalM := startSample + m
		ref := inAmp * math.Sin(2*math.Pi*testFreq*float64(globalM)/float64(SampleRate))
		err := float64(val) - ref
		if math.Abs(err) > maxErr {
			maxErr = math.Abs(err)
		}
		sumErrSq += err * err
		sumRefSq += ref * ref
	}
	snr := 10 * math.Log10(sumRefSq/sumErrSq)
	t.Logf("Resampler SNR: %.2f dB, Max Sample Error: %.1f", snr, maxErr)
	if snr < 30.0 {
		t.Errorf("Resampler spectral purity poor! SNR = %.2f dB (expected > 30 dB)", snr)
	}
}


func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// goertzelAmp returns the approximate peak amplitude (in int16 sample units)
// of the spectral component at exactly `freq` Hz in `samples`, assuming a
// sample rate of `rate`. Unlike a naive DFT, the Goertzel algorithm measures
// at an exact frequency (not a quantized bin), so it's robust to the off-bin
// case without needing windowing. The amplitude normalization (×2/N) returns
// the peak amplitude of a pure cosine at the target freq, which makes the
// figure directly comparable to the int16 sample range (±32768).
func goertzelAmp(samples []int16, rate int, freq float64) float64 {
	N := len(samples)
	k := freq * float64(N) / float64(rate)
	omega := 2 * math.Pi * k / float64(N)
	coeff := 2 * math.Cos(omega)
	var s0, s1, s2 float64
	for _, x := range samples {
		s0 = float64(x) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	// Standard Goertzel magnitude: sqrt(re² + im²), then normalize to peak
	// amplitude of an equivalent cosine (A = mag * 2 / N).
	re := s1 - s2*math.Cos(omega)
	im := s2 * math.Sin(omega)
	mag := math.Sqrt(re*re + im*im)
	if N == 0 {
		return 0
	}
	return mag * 2.0 / float64(N)
}