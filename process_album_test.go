package waxtap

import (
	"context"
	"errors"
	"testing"
)

// TestProcessAlbumValidation covers checks that run before ffmpeg is needed.
func TestProcessAlbumValidation(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("no inputs", func(t *testing.T) {
		if _, err := c.ProcessAlbum(ctx, nil, -14, TranscodeSpec{Format: FormatFLAC}); err == nil {
			t.Error("expected error for empty album")
		}
	})

	t.Run("copy rejected", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "a.flac", Output: "b.flac"}}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatCopy})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("copy album = %v, want ErrIncompatibleSpec", err)
		}
	})

	t.Run("same input/output rejected", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "same.flac", Output: "same.flac"}}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("same-path album = %v, want ErrIncompatibleSpec", err)
		}
	})

	t.Run("missing output path", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "a.flac", Output: ""}}
		if _, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC}); err == nil {
			t.Error("expected error for missing output path")
		}
	})

	t.Run("two tracks share an output", func(t *testing.T) {
		tracks := []AlbumTrack{
			{Input: "a.flac", Output: "out.flac"},
			{Input: "b.flac", Output: "out.flac"},
		}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("shared output = %v, want ErrIncompatibleSpec", err)
		}
	})

	t.Run("output overwrites another track's input", func(t *testing.T) {
		tracks := []AlbumTrack{
			{Input: "a.flac", Output: "b.flac"}, // would clobber track 2's source
			{Input: "b.flac", Output: "c.flac"},
		}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("cross-clobber = %v, want ErrIncompatibleSpec", err)
		}
	})
}
