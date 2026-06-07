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
