// Package mediatest provides pure-Go media fixtures for tests, replacing the
// ffmpeg lavfi generators the pre-WaxFlow suite relied on. It writes standard
// 16-bit PCM and IEEE float32 WAV bytes that the WaxFlow engine decodes, plus
// synthetic cover-art images, so the test suite needs no external tools. The
// float fixtures are the only ones that can hold samples past full scale,
// which is what the level-report tests need.
package mediatest

import (
	"encoding/binary"
	"fmt"
	"math"
)

// SineWAV returns a 16-bit little-endian PCM WAV of a 440 Hz sine at roughly
// -6 dBFS: seconds long, channels wide, 44100 Hz. Every channel carries the same
// tone. It is enough for probe, transcode, cut, and loudness tests.
func SineWAV(seconds, channels int) []byte {
	return ToneWAV(440.0, seconds, channels, 44100)
}

// ToneWAV returns a 16-bit PCM WAV of a sine at freqHz, at roughly -6 dBFS.
func ToneWAV(freqHz float64, seconds, channels, rate int) []byte {
	if rate <= 0 {
		rate = 44100
	}
	const amp = 0.5 // ~-6 dBFS
	return pcmWAV(seconds*rate, channels, rate, func(i int) float64 {
		return amp * math.Sin(2*math.Pi*freqHz*float64(i)/float64(rate))
	})
}

// QuietWithTransientWAV returns a 16-bit PCM WAV of a quiet 440 Hz sine (~-40
// dBFS) carrying one half-millisecond full-scale transient, 44100 Hz.
//
// The two are deliberately far apart: the transient puts the true peak at ~0 dBTP
// while the body keeps the integrated loudness near -41 LUFS, so a peak-capped
// normalization can take almost no gain and lands tens of LU short of a normal
// target. It is the fixture for the two peak policies; a plain sine cannot show
// the difference because its peak and loudness track each other.
//
// The transient is a half cycle of its own sine, not a window of the body's, so
// it reaches exactly full scale at its midpoint whatever the body frequency is. A
// window of the 440 Hz body would peak wherever its phase happened to land (0.97
// at 44100 Hz), leaving the fixture's whole purpose resting on constants that
// read as incidental.
func QuietWithTransientWAV(seconds, channels int) []byte {
	const (
		rate      = 44100
		bodyHz    = 440.0
		quietAmp  = 0.01   // ~-40 dBFS
		burstSecs = 0.0005 // long enough for the 4x-oversampled true-peak meter
	)
	frames := seconds * rate
	burstFrames := int(math.Round(burstSecs * rate))
	burstStart := frames / 2

	return pcmWAV(frames, channels, rate, func(i int) float64 {
		if i >= burstStart && i < burstStart+burstFrames {
			// A half cycle across the burst: sin sweeps 0 to pi, touching 1.0 midway.
			return math.Sin(math.Pi * float64(i-burstStart) / float64(burstFrames))
		}
		return quietAmp * math.Sin(2*math.Pi*bodyHz*float64(i)/float64(rate))
	})
}

// CrestWAV returns a 16-bit PCM WAV with a moderate crest factor: a ~-24 dBFS
// 440 Hz body carrying four half-millisecond full-scale transients per second,
// 44100 Hz. It measures near -23 LUFS at ~0 dBTP, about 23 dB of crest.
//
// It exists for the loudness-convergence tests, between the two fixtures that
// cannot serve there. A plain sine's peak and loudness track each other, so one
// normalization pass lands on the target and nothing iterates.
// [QuietWithTransientWAV] is the opposite extreme, roughly 41 dB of crest, where
// the limiter saturates before reaching a normal target; it is the saturation
// fixture, not the convergence one. This sits in the middle: one pass misses the
// target by more than the reporting tolerance, and correcting reaches it.
//
// The transients repeat rather than appearing once so that they contribute to the
// gated integrated loudness rather than reading as a single outlier, which is what
// makes the miss reproducible instead of incidental.
func CrestWAV(seconds, channels int) []byte {
	const (
		rate       = 44100
		bodyHz     = 440.0
		bodyAmp    = 0.06   // ~-24 dBFS
		burstSecs  = 0.0005 // long enough for the 4x-oversampled true-peak meter
		burstsPerS = 4
	)
	frames := seconds * rate
	burstFrames := int(math.Round(burstSecs * rate))
	period := rate / burstsPerS

	return pcmWAV(frames, channels, rate, func(i int) float64 {
		if p := i % period; p < burstFrames {
			// A half cycle across the burst, touching 1.0 midway, for the same reason
			// QuietWithTransientWAV uses one: the peak does not depend on body phase.
			return math.Sin(math.Pi * float64(p) / float64(burstFrames))
		}
		return bodyAmp * math.Sin(2*math.Pi*bodyHz*float64(i)/float64(rate))
	})
}

// HotFloatWAV returns an IEEE float32 WAV that clips an integer encode
// deterministically: a ~-8 dBFS 440 Hz sine with overs samples per channel
// planted past full scale at alternating +-6.0, 44100 Hz. A float container
// holds the overs as they stand, so a same-rate transcode to an integer
// output with no boosting gain must clamp exactly overs*channels samples;
// 16-bit fixtures cannot express this, which is why the clipping tests need
// a float one. The exact count holds only for that shape: a resample would
// ring the step discontinuities into extra overs, and a boosting gain would
// engage the limiter, which smears them. The overs sit ~15.6 dB past full
// scale so they still clip through the moderate attenuation a normalization
// pass applies to the ~-10 LUFS body.
func HotFloatWAV(seconds, channels, overs int) []byte {
	const (
		rate   = 44100
		bodyHz = 440.0
		amp    = 0.4 // ~-8 dBFS
		over   = 6.0 // ~+15.6 dB past full scale
		// First over's frame and the spacing between them, mirroring WaxFlow's own
		// clipping fixture: far enough apart that the count reads against the tone.
		oversStart, oversStep = 100, 37
	)
	frames := seconds * rate
	if need := oversStart + max(overs-1, 0)*oversStep + 1; frames < need {
		panic(fmt.Sprintf("mediatest.HotFloatWAV: %d frames cannot hold %d overs (need %d)", frames, overs, need))
	}
	return floatWAV(frames, channels, rate, func(i int) float64 {
		if k := i - oversStart; k >= 0 && k%oversStep == 0 && k/oversStep < overs {
			if (k/oversStep)%2 == 1 {
				return -over
			}
			return over
		}
		return amp * math.Sin(2*math.Pi*bodyHz*float64(i)/float64(rate))
	})
}

// IntersampleHotWAV returns an IEEE float32 WAV whose stored samples all sit
// under full scale while the waveform between them crosses it: a quarter-rate
// stereo sine at amplitude 1.2 phased off its crests, stored peak ~0.85, true
// peak 1.2 (the shape WaxFlow's own clipping tests use). A quantizer clamps
// nothing, so only the output true-peak meter reports the over.
func IntersampleHotWAV(seconds, channels int) []byte {
	const rate = 44100
	freq := float64(rate) / 4
	return floatWAV(seconds*rate, channels, rate, func(i int) float64 {
		return 1.2 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)+math.Pi/4)
	})
}

// floatWAV builds an IEEE float32 WAV from a per-frame sample function, which
// may exceed [-1, 1]. Every channel carries the same signal.
func floatWAV(frames, channels, rate int, sampleAt func(i int) float64) []byte {
	if channels < 1 {
		channels = 1
	}
	data := make([]byte, frames*channels*4)
	off := 0
	for i := range frames {
		s := math.Float32bits(float32(sampleAt(i)))
		for range channels {
			binary.LittleEndian.PutUint32(data[off:], s)
			off += 4
		}
	}
	return wavContainer(data, channels, rate, 32)
}

// pcmWAV builds a 16-bit WAV from a per-frame sample function in [-1, 1]. Every
// channel carries the same signal.
func pcmWAV(frames, channels, rate int, sampleAt func(i int) float64) []byte {
	if channels < 1 {
		channels = 1
	}
	data := make([]byte, frames*channels*2)
	off := 0
	for i := range frames {
		s := int16(math.Round(sampleAt(i) * math.MaxInt16))
		for range channels {
			binary.LittleEndian.PutUint16(data[off:], uint16(s))
			off += 2
		}
	}
	return wavContainer(data, channels, rate, 16)
}

// wavContainer wraps raw interleaved PCM in a canonical 44-byte RIFF/WAVE
// header. 32-bit samples are IEEE float (format tag 3); narrower ones are
// integer PCM (tag 1), the only two layouts this package writes.
func wavContainer(pcm []byte, channels, rate, bits int) []byte {
	tag := uint16(1) // WAVE_FORMAT_PCM
	if bits == 32 {
		tag = 3 // WAVE_FORMAT_IEEE_FLOAT
	}
	blockAlign := channels * bits / 8
	byteRate := rate * blockAlign
	buf := make([]byte, 44+len(pcm))
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+len(pcm)))
	copy(buf[8:], "WAVE")
	copy(buf[12:], "fmt ")
	// A 16-byte fmt chunk for both tags: format 3 canonically carries a fact
	// chunk too, but WaxFlow's demuxer does not need one, and WaxFlow's own
	// float test fixture omits it the same way.
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], tag)
	binary.LittleEndian.PutUint16(buf[22:], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(buf[34:], uint16(bits))
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(len(pcm)))
	copy(buf[44:], pcm)
	return buf
}
