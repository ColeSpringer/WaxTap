// Package mediatest provides pure-Go audio fixtures for tests, replacing the
// ffmpeg lavfi generators the pre-WaxFlow suite relied on. It writes standard
// 16-bit PCM WAV bytes that the WaxFlow engine decodes, so the test suite needs
// no external tools.
package mediatest

import (
	"encoding/binary"
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

// wavContainer wraps raw interleaved PCM in a canonical 44-byte RIFF/WAVE header.
func wavContainer(pcm []byte, channels, rate, bits int) []byte {
	blockAlign := channels * bits / 8
	byteRate := rate * blockAlign
	buf := make([]byte, 44+len(pcm))
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+len(pcm)))
	copy(buf[8:], "WAVE")
	copy(buf[12:], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(buf[20:], 1)  // PCM format
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
