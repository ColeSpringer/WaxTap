package download

import (
	"errors"
	"io"
	"testing"
)

type failWriter struct{ err error }

func (f failWriter) Write(p []byte) (int, error) { return 0, f.err }

func TestCountingWriter_RecordsSinkError(t *testing.T) {
	boom := errors.New("no space left on device")
	cw := &countingWriter{w: failWriter{err: boom}, rep: newProgress(nil, 0)}
	if _, err := cw.Write([]byte("data")); !errors.Is(err, boom) {
		t.Fatalf("Write err = %v, want the sink error", err)
	}
	if !errors.Is(cw.werr, boom) {
		t.Fatalf("werr = %v, want the sink error recorded", cw.werr)
	}
}

func TestCountingWriter_NoErrorOnSuccess(t *testing.T) {
	cw := &countingWriter{w: io.Discard, rep: newProgress(nil, 0)}
	if _, err := cw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if cw.werr != nil {
		t.Errorf("werr = %v, want nil on a successful write", cw.werr)
	}
}
