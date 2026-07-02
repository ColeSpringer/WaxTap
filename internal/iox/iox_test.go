package iox

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

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
