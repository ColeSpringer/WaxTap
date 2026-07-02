package transcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v2/waxerr"
)

// ffprobe accepts an empty raw FLAC by extension and reports an audio stream
// with zero channels and sample rate. Probe must reject that stream.
func TestProbe_RejectsZeroByteFLAC(t *testing.T) {
	r := newTestRunner(t)
	path := filepath.Join(t.TempDir(), "empty.flac")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := r.Probe(context.Background(), path)
	if !errors.Is(err, waxerr.ErrUnsupportedInput) {
		t.Fatalf("Probe err = %v, want ErrUnsupportedInput", err)
	}
	if !strings.Contains(err.Error(), "unsupported or unreadable input") {
		t.Errorf("message = %q, want it to mention unsupported/unreadable input", err)
	}
	// The clean classification must not leak ffmpeg/filtergraph internals.
	for _, leak := range []string{"filtergraph", "[out]", "encoder", "0:a:0"} {
		if strings.Contains(strings.ToLower(err.Error()), leak) {
			t.Errorf("message %q leaks ffmpeg detail %q", err, leak)
		}
	}
}

// Transcode does not probe every output format, so ffmpeg's empty-output failure
// is the backstop for degenerate inputs.
func TestTranscode_ZeroByteFLACBackstop(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "empty.flac")
	if err := os.WriteFile(in, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mp3")
	_, err := r.Transcode(context.Background(), in, out, Spec{Codec: CodecMP3})
	if !errors.Is(err, waxerr.ErrUnsupportedInput) {
		t.Fatalf("Transcode err = %v, want ErrUnsupportedInput", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "could not open encoder") {
		t.Errorf("message %q leaks the ffmpeg stderr tail", err)
	}
}

func TestEmptyDecodeFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"could-not-open-encoder", &RunError{ExitCode: 183, Stderr: "[aost] Could not open encoder before EOF\n"}, true},
		{"nothing-written", &RunError{ExitCode: 1, Stderr: "Nothing was written into output file, because ...\n"}, true},
		{"output-empty", &RunError{ExitCode: 1, Stderr: "Output file is empty, nothing was encoded\n"}, true},
		{"ordinary-failure", &RunError{ExitCode: 1, Stderr: "Invalid argument\n"}, false},
		{"not-a-runerror", errors.New("some other error"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := emptyDecodeFailure(tc.err); got != tc.want {
				t.Errorf("emptyDecodeFailure = %v, want %v", got, tc.want)
			}
		})
	}
}
