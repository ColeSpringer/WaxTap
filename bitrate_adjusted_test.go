package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// outputExtFor names the file extension a format's usual container takes, so
// a table of formats can write one file each.
func outputExtFor(f TranscodeFormat) string {
	switch f {
	case FormatMP3:
		return ".mp3"
	case FormatAAC:
		return ".m4a"
	case FormatOpus:
		return ".opus"
	default:
		return ".flac"
	}
}

// Each encoder has its own rate range and grid. A request it cannot use
// exactly is snapped to the nearest it supports and reported; one it can use
// is silent. WaxTap keeps no copy of those tables: the figure is read off
// WaxFlow's own plan of the encode.
func TestProcessReportsTheRateTheEncoderRanAt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	in := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(in, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		format    TranscodeFormat
		requested int
		delivered int // 0 means no adjustment expected
	}{
		{"mp3 off the CBR table snaps", FormatMP3, 200000, 192000},
		{"mp3 under the MPEG-1 table floor snaps up", FormatMP3, 8000, 32000},
		{"mp3 on the table is silent", FormatMP3, 192000, 0},
		{"opus over the frame cap clamps", FormatOpus, 512000, 510000},
		{"aac under 8 kb/s per channel floors", FormatAAC, 8000, 16000},
		{"opus in range is silent", FormatOpus, 96000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Process(ctx, ProcessRequest{Input: in, ProcessSpec: ProcessSpec{
				Transcode: &TranscodeSpec{Format: tc.format, Bitrate: tc.requested},
				Output:    ToFile(filepath.Join(dir, tc.name+outputExtFor(tc.format))),
			}})
			if err != nil {
				t.Fatal(err)
			}
			got, ok := findWarning(res.Warnings, WarnBitrateAdjusted)
			if tc.delivered == 0 {
				if ok {
					t.Fatalf("unexpected warning %v", got)
				}
				return
			}
			if !ok || !strings.Contains(got.Detail, strconv.Itoa(tc.delivered)) {
				t.Fatalf("warnings = %v, want bitrate-adjusted naming %d", res.Warnings, tc.delivered)
			}
			if !strings.Contains(got.Detail, strconv.Itoa(tc.requested)) {
				t.Errorf("detail = %q, want the requested %d named too", got.Detail, tc.requested)
			}
		})
	}

	// The plausibility floor still refuses a rate no encoder could mean, before
	// the plan is consulted.
	if err := ValidateProcessSpec(ProcessSpec{
		Output:    ToFile(filepath.Join(dir, "x.mp3")),
		Transcode: &TranscodeSpec{Format: FormatMP3, Bitrate: 500},
	}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("ValidateProcessSpec(500 b/s) = %v, want ErrIncompatibleSpec", err)
	}

	// A rate under the encoder's own floor is refused by the plan, which runs
	// before anything is written: nothing is left behind.
	out := filepath.Join(dir, "floor.opus")
	_, err := c.Process(ctx, ProcessRequest{Input: in, ProcessSpec: ProcessSpec{
		Transcode: &TranscodeSpec{Format: FormatOpus, Bitrate: 2000},
		Output:    ToFile(out),
	}})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("Process at 2000 b/s Opus = %v, want ErrIncompatibleSpec", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Error("the refusal came after a file was written; it must precede the encode")
	}
}
