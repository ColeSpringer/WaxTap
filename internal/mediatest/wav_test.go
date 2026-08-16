package mediatest

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestSineWAVHeader(t *testing.T) {
	for _, ch := range []int{1, 2, 6} {
		b := SineWAV(1, ch)
		if !bytes.Equal(b[0:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WAVE")) {
			t.Fatalf("ch=%d: not a RIFF/WAVE file", ch)
		}
		gotCh := binary.LittleEndian.Uint16(b[22:24])
		rate := binary.LittleEndian.Uint32(b[24:28])
		bits := binary.LittleEndian.Uint16(b[34:36])
		if int(gotCh) != ch || rate != 44100 || bits != 16 {
			t.Errorf("ch=%d: header ch=%d rate=%d bits=%d, want %d/44100/16", ch, gotCh, rate, bits, ch)
		}
		// 1s of 16-bit PCM at 44100 Hz for ch channels, plus the 44-byte header.
		if want := 44 + 44100*ch*2; len(b) != want {
			t.Errorf("ch=%d: len = %d, want %d", ch, len(b), want)
		}
	}
}

func TestHotFloatWAVHeader(t *testing.T) {
	b := HotFloatWAV(1, 2, 13)
	if !bytes.Equal(b[0:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WAVE")) {
		t.Fatal("not a RIFF/WAVE file")
	}
	if tag := binary.LittleEndian.Uint16(b[20:22]); tag != 3 {
		t.Errorf("format tag = %d, want 3 (IEEE float)", tag)
	}
	gotCh := binary.LittleEndian.Uint16(b[22:24])
	rate := binary.LittleEndian.Uint32(b[24:28])
	bits := binary.LittleEndian.Uint16(b[34:36])
	if gotCh != 2 || rate != 44100 || bits != 32 {
		t.Errorf("header ch=%d rate=%d bits=%d, want 2/44100/32", gotCh, rate, bits)
	}
	// 1s of float32 PCM at 44100 Hz for 2 channels, plus the 44-byte header.
	if want := 44 + 44100*2*4; len(b) != want {
		t.Errorf("len = %d, want %d", len(b), want)
	}
}

func TestToneWAVDistinctFrequencies(t *testing.T) {
	// Different tones must produce different samples (guards against a constant).
	if bytes.Equal(ToneWAV(220, 1, 1, 44100), ToneWAV(880, 1, 1, 44100)) {
		t.Error("220 Hz and 880 Hz tones produced identical bytes")
	}
}
