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

func TestFrontsOnlyWAVLeavesTheRearSilent(t *testing.T) {
	b := FrontsOnlyWAV(1, 6)
	if want := 44 + 44100*6*2; len(b) != want {
		t.Fatalf("len = %d, want %d", len(b), want)
	}
	front, rear := false, false
	for off := 44; off+12 <= len(b); off += 12 {
		for ch := range 6 {
			s := int16(binary.LittleEndian.Uint16(b[off+ch*2:]))
			switch {
			case ch < 2 && s != 0:
				front = true
			case ch >= 2 && s != 0:
				rear = true
			}
		}
	}
	if !front || rear {
		t.Errorf("front carries signal = %v, rear carries signal = %v; want true/false", front, rear)
	}
}

// A segment with no frequency is digital silence, which is what makes a
// removed span detectable in a cut's output.
func TestSegmentedWAVSilentSegment(t *testing.T) {
	const rate = 48000
	b := SegmentedWAV(2, rate, Segment{Seconds: 1, FreqHz: 440}, Segment{Seconds: 1}, Segment{Seconds: 1, FreqHz: 880})
	if want := 44 + 3*rate*2*2; len(b) != want {
		t.Fatalf("len = %d, want %d (3 s of stereo 16-bit at %d Hz plus the header)", len(b), want, rate)
	}
	if gotRate := binary.LittleEndian.Uint32(b[24:28]); gotRate != rate {
		t.Errorf("rate = %d, want %d", gotRate, rate)
	}
	mid := b[44+rate*2*2 : 44+2*rate*2*2]
	for i := 0; i < len(mid); i += 2 {
		if s := int16(binary.LittleEndian.Uint16(mid[i:])); s != 0 {
			t.Fatalf("frame %d of the middle second = %d, want silence", i/4, s)
		}
	}
	// The segments around it are not silent, so the fixture is not silent throughout.
	first := b[44 : 44+rate*2*2]
	if bytes.Equal(first, make([]byte, len(first))) {
		t.Error("the first second is silent too")
	}
}
