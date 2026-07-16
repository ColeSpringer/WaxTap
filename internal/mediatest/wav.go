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
	if channels < 1 {
		channels = 1
	}
	if rate <= 0 {
		rate = 44100
	}
	frames := seconds * rate
	const amp = 0.5 // ~-6 dBFS
	data := make([]byte, frames*channels*2)
	off := 0
	for i := 0; i < frames; i++ {
		v := amp * math.Sin(2*math.Pi*freqHz*float64(i)/float64(rate))
		s := int16(math.Round(v * math.MaxInt16))
		for c := 0; c < channels; c++ {
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
