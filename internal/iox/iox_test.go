package iox

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// shortBody delivers data and then fails with err, as a body whose connection
// dropped does.
type shortBody struct {
	data []byte
	err  error
}

func (b *shortBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func TestReadAllCapped(t *testing.T) {
	// Under the cap: the whole body is returned.
	if b, err := ReadAllCapped(bytes.NewReader([]byte("hello")), 16, "x"); err != nil || string(b) != "hello" {
		t.Fatalf("under cap = (%q, %v), want (\"hello\", nil)", b, err)
	}

	// Exactly at the cap: kept whole, not treated as truncated.
	body := bytes.Repeat([]byte{'a'}, 8)
	if b, err := ReadAllCapped(bytes.NewReader(body), 8, "x"); err != nil || len(b) != 8 {
		t.Fatalf("at cap = (%d bytes, %v), want (8, nil)", len(b), err)
	}

	// One byte over the cap: rejected with a clear, classifiable error.
	_, err := ReadAllCapped(bytes.NewReader(bytes.Repeat([]byte{'a'}, 9)), 8, "player resource")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("over cap err = %v, want ErrResponseTooLarge", err)
	}
	if !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "player resource") {
		t.Errorf("over cap err = %v, want it to name the label and truncation", err)
	}
}

// A body that fails part way hands back the bytes read before the failure with
// the error, so a caller that can use a prefix (a SABR round salvaging its whole
// segments) has it; one that cannot checks the error first, as every caller did
// before, and sees no difference.
func TestReadAllCapped_PrefixReturnedWithReadError(t *testing.T) {
	cause := errors.New("connection reset")
	b, err := ReadAllCapped(&shortBody{data: []byte("partial"), err: cause}, 64, "x")
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want the body's error", err)
	}
	if string(b) != "partial" {
		t.Errorf("prefix = %q, want \"partial\"", b)
	}
	// io.ErrUnexpectedEOF is a read error like any other here, not a clean end.
	b, err = ReadAllCapped(&shortBody{data: []byte("cut"), err: io.ErrUnexpectedEOF}, 64, "x")
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(b) != "cut" {
		t.Errorf("= (%q, %v), want (\"cut\", unexpected EOF)", b, err)
	}
}

// overfullBody hands back more than the cap in one read, with an error riding
// along: the cap verdict comes first, since an over-cap body is no prefix.
type overfullBody struct{ n int }

func (b *overfullBody) Read(p []byte) (int, error) {
	if b.n == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, bytes.Repeat([]byte{'x'}, b.n))
	b.n -= n
	return n, io.ErrUnexpectedEOF
}

func TestReadAllCapped_CapBeatsReadError(t *testing.T) {
	b, err := ReadAllCapped(&overfullBody{n: 9}, 8, "x")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	if b != nil {
		t.Errorf("got %d bytes with the cap error, want none", len(b))
	}
}
