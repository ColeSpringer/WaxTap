package sabr

import (
	"bytes"
	"testing"
)

func TestUMPVarintRoundTrip(t *testing.T) {
	// Cover each length tier and its boundaries.
	cases := []struct {
		v       uint64
		wantLen int
	}{
		{0, 1},
		{1, 1},
		{127, 1}, // 2^7-1, last 1-byte value
		{128, 2}, // first 2-byte value
		{300, 2},
		{16383, 2},      // 2^14-1
		{16384, 3},      // first 3-byte value
		{2097151, 3},    // 2^21-1
		{2097152, 4},    // first 4-byte value
		{268435455, 4},  // 2^28-1
		{268435456, 5},  // first 5-byte value
		{4294967295, 5}, // 2^32-1
	}
	for _, tc := range cases {
		enc := umpVarint(tc.v)
		if len(enc) != tc.wantLen {
			t.Errorf("umpVarint(%d) used %d bytes, want %d (%x)", tc.v, len(enc), tc.wantLen, enc)
		}
		r := newUMPReader(enc)
		got, err := r.readVarint()
		if err != nil {
			t.Errorf("readVarint(%x): %v", enc, err)
			continue
		}
		if got != tc.v {
			t.Errorf("readVarint(%x) = %d, want %d", enc, got, tc.v)
		}
		if r.pos != len(enc) {
			t.Errorf("readVarint(%d) consumed %d bytes, want %d", tc.v, r.pos, len(enc))
		}
	}
}

// TestUMPVarintWireVectors pins the UMP varint against YouTube's real wire bytes
// using hand-written literals (independent of umpVarint), so the inverted layout
// that produced the false "status 2" can never silently return. The round-trip
// test alone could not catch the bug because encoder and decoder shared the
// inversion. Vectors verified against LuanRT/googlevideo UmpReader.ts/UmpWriter.ts.
func TestUMPVarintWireVectors(t *testing.T) {
	cases := []struct {
		v    uint64
		wire []byte
	}{
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x02}},
		{259, []byte{0x83, 0x04}},
		{16383, []byte{0xBF, 0xFF}},
		{16384, []byte{0xC0, 0x00, 0x02}},
		{32769, []byte{0xC1, 0x00, 0x04}}, // a 32 KB+1 MEDIA size, like the real capture
		{65536, []byte{0xC0, 0x00, 0x08}},
		{2097151, []byte{0xDF, 0xFF, 0xFF}},
		{2097152, []byte{0xE0, 0x00, 0x00, 0x02}},
		{268435456, []byte{0xF0, 0x00, 0x00, 0x00, 0x10}},
		{4294967295, []byte{0xF0, 0xFF, 0xFF, 0xFF, 0xFF}},
	}
	for _, tc := range cases {
		if got := umpVarint(tc.v); !bytes.Equal(got, tc.wire) {
			t.Errorf("umpVarint(%d) = % x, want % x", tc.v, got, tc.wire)
		}
		r := newUMPReader(tc.wire)
		got, err := r.readVarint()
		if err != nil {
			t.Errorf("readVarint(% x): %v", tc.wire, err)
			continue
		}
		if got != tc.v {
			t.Errorf("readVarint(% x) = %d, want %d", tc.wire, got, tc.v)
		}
		if r.pos != len(tc.wire) {
			t.Errorf("readVarint(%d) consumed %d bytes, want %d", tc.v, r.pos, len(tc.wire))
		}
	}
}

func TestUMPReaderPartsSkipsUnknownBySize(t *testing.T) {
	const unknownPart = 99
	body := concat(
		umpFrame(partStreamProtection, []byte("AA")),
		umpFrame(unknownPart, []byte("skip-me-by-size")),
		umpFrame(partMediaEnd, []byte{0x07}),
	)
	r := newUMPReader(body)

	want := []umpPart{
		{Type: partStreamProtection, Payload: []byte("AA")},
		{Type: unknownPart, Payload: []byte("skip-me-by-size")},
		{Type: partMediaEnd, Payload: []byte{0x07}},
	}
	for i, w := range want {
		part, ok, err := r.next()
		if err != nil || !ok {
			t.Fatalf("part %d: ok=%v err=%v", i, ok, err)
		}
		if part.Type != w.Type || !bytes.Equal(part.Payload, w.Payload) {
			t.Fatalf("part %d = {type:%d payload:%q}, want {type:%d payload:%q}", i, part.Type, part.Payload, w.Type, w.Payload)
		}
	}
	// The unknown part was framed correctly (skipped by size), so the stream ends
	// cleanly after the last known part.
	if _, ok, err := r.next(); ok || err != nil {
		t.Fatalf("end of stream: ok=%v err=%v, want clean end", ok, err)
	}
}

func TestLeadingVarintSplitsMediaPayload(t *testing.T) {
	media := []byte("rawmediabytes")
	payload := append(umpVarint(259), media...) // header_id 259 spans two bytes
	id, rest, err := leadingVarint(payload)
	if err != nil {
		t.Fatal(err)
	}
	if id != 259 {
		t.Errorf("header_id = %d, want 259", id)
	}
	if !bytes.Equal(rest, media) {
		t.Errorf("media = %q, want %q", rest, media)
	}
}

func TestUMPReaderTruncated(t *testing.T) {
	// A part claiming 10 bytes of payload but only 3 present.
	body := concat(umpVarint(partMedia), umpVarint(10), []byte("abc"))
	r := newUMPReader(body)
	if _, _, err := r.next(); err == nil {
		t.Fatal("next() on truncated part = nil error, want truncation error")
	}
}
