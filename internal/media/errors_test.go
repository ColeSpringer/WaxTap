package media

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wferr "github.com/colespringer/waxflow/waxerr"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// TestClassifyEngineError walks every row of the mapping, including the two I/O
// codes: sending a genuine read or write failure to "invalid request" would be
// wrong, and the exit code is the contract.
func TestClassifyEngineError(t *testing.T) {
	cases := []struct {
		code   wferr.Code
		assert func(*testing.T, error)
	}{
		{wferr.CodeUnsupportedFormat, wantSentinel(waxerr.ErrIncompatibleSpec)},
		{wferr.CodeInvalidRequest, wantSentinel(waxerr.ErrIncompatibleSpec)},
		{wferr.CodePayloadTooLarge, wantSentinel(waxerr.ErrIncompatibleSpec)},
		{wferr.CodeUnsupportedSource, wantSentinel(waxerr.ErrUnsupportedInput)},
		{wferr.CodeSourceUnreadable, func(t *testing.T, got error) {
			pe, ok := errors.AsType[*fs.PathError](got)
			if !ok {
				t.Fatalf("got %T (%v), want *fs.PathError so it exits 10", got, got)
			}
			if pe.Path != "in.flac" || pe.Op != "read" {
				t.Errorf("PathError = %+v, want a read of in.flac", pe)
			}
		}},
		{wferr.CodeOutputUnwritable, func(t *testing.T, got error) {
			oe, ok := errors.AsType[*tempfile.OutputError](got)
			if !ok {
				t.Fatalf("got %T (%v), want *tempfile.OutputError so it exits 10", got, got)
			}
			if oe.Op != "write" {
				t.Errorf("OutputError.Op = %q, want write", oe.Op)
			}
			if !strings.Contains(got.Error(), "out.flac") {
				t.Errorf("message = %q, want the output path named", got)
			}
		}},
		{wferr.CodeCanceled, func(t *testing.T, got error) {
			if !errors.Is(got, context.Canceled) {
				t.Errorf("got %v, want context.Canceled so it exits 130", got)
			}
		}},
		// Deliberately unmapped, each for its own reason: internal is exit 1 by
		// definition, not-found comes only from WaxFlow's HTTP client, and overloaded
		// describes a server WaxTap does not run.
		{wferr.CodeInternal, wantUnchanged},
		{wferr.CodeNotFound, wantUnchanged},
		{wferr.CodeOverloaded, wantUnchanged},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			src := wferr.New(tc.code, "boom")
			tc.assert(t, classifyEngineError(src, "in.flac", "out.flac"))
		})
	}

	if got := classifyEngineError(nil, "in", "out"); got != nil {
		t.Errorf("classifyEngineError(nil) = %v, want nil", got)
	}
	// A context error anywhere in the chain already reports the cancellation, so it
	// is returned as-is rather than re-wrapped.
	canceled := context.Canceled
	if got := classifyEngineError(canceled, "in", "out"); got != canceled {
		t.Errorf("got %v, want the context error untouched", got)
	}
	// With no path to name, an I/O code stays unmapped rather than reporting a path
	// error about nothing.
	unreadable := wferr.New(wferr.CodeSourceUnreadable, "boom")
	if got := classifyEngineError(unreadable, "", ""); got != unreadable {
		t.Errorf("got %v, want the error unchanged when there is no path", got)
	}
}

func wantSentinel(want error) func(*testing.T, error) {
	return func(t *testing.T, got error) {
		if !errors.Is(got, want) {
			t.Errorf("got %v, want errors.Is %v", got, want)
		}
	}
}

func wantUnchanged(t *testing.T, got error) {
	if _, ok := errors.AsType[*wferr.Error](got); !ok {
		t.Errorf("got %T (%v), want the WaxFlow error passed through", got, got)
	}
	if errors.Is(got, waxerr.ErrIncompatibleSpec) || errors.Is(got, waxerr.ErrUnsupportedInput) {
		t.Errorf("got %v, want no WaxTap sentinel attached", got)
	}
}

// classifyInputError keeps the ErrUnsupportedInput default its call sites carried
// for anything it cannot map, so an unclassified probe failure does not silently
// move from exit 2 to exit 1.
func TestClassifyInputError(t *testing.T) {
	if got := classifyInputError(wferr.New(wferr.CodeInternal, "boom"), "in.flac"); !errors.Is(got, waxerr.ErrUnsupportedInput) {
		t.Errorf("got %v, want the ErrUnsupportedInput default", got)
	}
	// WaxFlow's malformed() helpers all carry CodeUnsupportedFormat, so at a
	// read-only site it describes the file, not the spec.
	got := classifyInputError(wferr.New(wferr.CodeUnsupportedFormat, "mp4: fragment sample runs past end of source"), "in.m4a")
	if !errors.Is(got, waxerr.ErrUnsupportedInput) {
		t.Errorf("got %v, want ErrUnsupportedInput: a truncated file is not an incompatible spec", got)
	}
	if errors.Is(got, waxerr.ErrIncompatibleSpec) {
		t.Errorf("got %v, want no spec sentinel on a read failure", got)
	}
	// A code that is unambiguous still maps normally.
	if _, ok := errors.AsType[*fs.PathError](classifyInputError(wferr.New(wferr.CodeSourceUnreadable, "boom"), "in.flac")); !ok {
		t.Error("want the I/O mapping to survive at a read-only site with a path")
	}
	if got := classifyInputError(nil, "in.flac"); got != nil {
		t.Errorf("classifyInputError(nil) = %v, want nil", got)
	}
}

// F13 end to end: ALAC holds mono and stereo only, and the refusal comes from the
// encoder, which used to reach the CLI as a bare error at exit 1 while every
// other unsupported-spec case exits 2.
func TestTranscodeMultichannelALACIsIncompatibleSpec(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "surround.wav")
	if err := os.WriteFile(src, mediatest.SineWAV(1, 6), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(RunnerConfig{})
	_, err := r.Transcode(context.Background(), src, filepath.Join(dir, "out.m4a"), Spec{Codec: CodecALAC})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("6-channel ALAC = %v, want ErrIncompatibleSpec", err)
	}
	if !strings.Contains(err.Error(), "channels") {
		t.Errorf("message = %q, want it to name the channel count", err)
	}
}

// The log wrapper demotes exactly one WaxFlow record. A blanket demotion would
// silence the upstream warnings WaxTap has not learned to detect, which is the
// same defect one release later.
func TestDemoteImplicitDownmix(t *testing.T) {
	emit := func(level slog.Level, msg string) string {
		var buf bytes.Buffer
		h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})
		slog.New(demoteImplicitDownmix{h}).Warn(msg, "source", 6, "out", 2)
		return buf.String()
	}

	if got := emit(slog.LevelWarn, implicitDownmixMsg); got != "" {
		t.Errorf("at WARN the demoted record still printed: %q", got)
	}
	got := emit(slog.LevelDebug, implicitDownmixMsg)
	if !strings.Contains(got, "level=DEBUG") || !strings.Contains(got, implicitDownmixMsg) {
		t.Errorf("under --verbose the record should appear at DEBUG: %q", got)
	}
	other := emit(slog.LevelWarn, "some future upstream warning")
	if !strings.Contains(other, "level=WARN") {
		t.Errorf("an unrelated WaxFlow WARN must pass through unchanged: %q", other)
	}
	// Attributes survive the wrapper, which has to rebuild itself in WithAttrs.
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.New(demoteImplicitDownmix{h}).With("job", "x").Warn("some future upstream warning")
	if !strings.Contains(buf.String(), "job=x") {
		t.Errorf("WithAttrs dropped attributes: %q", buf.String())
	}
}

// classifyInputError must not flatten the codes that mean something other than
// bad media. An identity test against classifyEngineError used to do exactly
// that, because that function returns some errors untouched by design.
func TestClassifyInputErrorKeepsNonMediaCodes(t *testing.T) {
	// A cancellation stays a cancellation: wrapping it as bad input would sever
	// errors.Is and move a Ctrl-C from exit 130 to exit 2.
	canceled := classifyInputError(wferr.New(wferr.CodeCanceled, "stopped"), "")
	if !errors.Is(canceled, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", canceled)
	}
	if errors.Is(canceled, waxerr.ErrUnsupportedInput) {
		t.Errorf("got %v, want no bad-input sentinel on a cancellation", canceled)
	}
	if got := classifyInputError(wferr.New(wferr.CodeInvalidRequest, "boom"), ""); !errors.Is(got, waxerr.ErrIncompatibleSpec) {
		t.Errorf("got %v, want ErrIncompatibleSpec", got)
	}
	// With no path there is no path error to build, so it keeps the read-only
	// default rather than falling through to exit 1.
	if got := classifyInputError(wferr.New(wferr.CodeSourceUnreadable, "boom"), ""); !errors.Is(got, waxerr.ErrUnsupportedInput) {
		t.Errorf("got %v, want the pathless I/O code to keep the input default", got)
	}
}
