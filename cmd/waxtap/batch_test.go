package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/media"
)

func TestExtPossiblyCodec(t *testing.T) {
	cases := []struct {
		ext, family string
		want        bool
	}{
		{".flac", "mp3", false}, {".flac", "flac", true},
		{".wav", "mp3", false}, {".wav", "flac", false}, // PCM is not a comparable target family.
		{".mp3", "mp3", true},
		{".m4a", "aac", true}, {".m4a", "alac", true}, {".m4a", "opus", false}, // ambiguous container
		{".ogg", "opus", true}, {".ogg", "vorbis", true}, {".ogg", "mp3", false},
		{".webm", "opus", true}, {".webm", "aac", false},
		{".mka", "aac", true}, {".mka", "flac", true}, // matroska is general-purpose
		{".xyz", "mp3", true}, // unknown: probe rather than guess
		// HE-AAC is an AAC-family resident of the same containers.
		{".m4a", "he-aac", true}, {".m4b", "he-aac", true}, {".aac", "he-aac", true},
		{".flac", "he-aac", false},
		// WavPack and APE fit only their own extensions.
		{".wv", "wavpack", true}, {".wv", "flac", false},
		{".ape", "ape", true}, {".ape", "wavpack", false},
	}
	for _, c := range cases {
		if got := extPossiblyCodec(c.ext, c.family); got != c.want {
			t.Errorf("extPossiblyCodec(%q,%q) = %v, want %v", c.ext, c.family, got, c.want)
		}
	}
	// A container WaxTap only reads never matches a target family, under any
	// of the engine's spellings for it (media.DecodeOnlyExts is the one table).
	for _, ext := range media.DecodeOnlyExts() {
		for _, family := range []string{"aac", "wavpack", "flac", "mp3"} {
			if extPossiblyCodec("."+ext, family) {
				t.Errorf("extPossiblyCodec(%q, %q) = true, want false (decode-only container)", "."+ext, family)
			}
		}
	}
}

// TestMatchesTargetFamilyHEAAC pins the AAC/HE-AAC asymmetry: an aac target
// leaves an HE-AAC file alone (WaxFlow copies it under its own identity, so
// "matches" avoids a lossy LC re-encode), while an he-aac target on an AAC-LC
// file is a real encode request and must not match.
func TestMatchesTargetFamilyHEAAC(t *testing.T) {
	cases := []struct {
		codec     string
		container string
		outExt    string // "" takes the format's canonical extension
		tf        waxtap.TranscodeFormat
		want      bool
	}{
		{codec: "he-aac", tf: waxtap.FormatAAC, want: true},
		{codec: "aac", tf: waxtap.FormatAAC, want: true},
		{codec: "he-aac", tf: waxtap.FormatHEAAC, want: true},
		{codec: "aac", tf: waxtap.FormatHEAAC, want: false},
		{codec: "wavpack", tf: waxtap.FormatWavPack, want: true},
		{codec: "ape", tf: waxtap.FormatAPE, want: true},
		{codec: "wavpack", tf: waxtap.FormatAPE, want: false},
		{codec: "wma", tf: waxtap.FormatAAC, want: false},
		{codec: "musepack", tf: waxtap.FormatAAC, want: false},
		// PCM belongs to its container, not to a codec family: the same
		// samples are RIFF in a WAV and big-endian in an AIFF, so both the
		// codec and the container have to match.
		{codec: "pcm_s16le", container: "wav", tf: waxtap.FormatWAV, want: true},
		{codec: "pcm_s16be", container: "aiff", tf: waxtap.FormatAIFF, want: true},
		{codec: "pcm_s16be", container: "aifc", tf: waxtap.FormatAIFF, want: true},
		{codec: "pcm_s16le", container: "wav", tf: waxtap.FormatAIFF, want: false},
		{codec: "pcm_s16be", container: "aiff", tf: waxtap.FormatWAV, want: false},
		// PCM carried in an MP4 is not a file either target already holds.
		{codec: "pcm_s16le", container: "mp4", tf: waxtap.FormatWAV, want: false},
		{codec: "flac", container: "flac", tf: waxtap.FormatWAV, want: false},
		// The answer for PCM is a verbatim byte copy, so the output's own
		// container has to agree too: a WAV asked for under a Matroska name
		// is a conversion, not a file that already exists.
		{codec: "pcm_s16le", container: "wav", outExt: "mka", tf: waxtap.FormatWAV, want: false},
		{codec: "pcm_s16le", container: "wav", outExt: "mp4", tf: waxtap.FormatWAV, want: false},
		{codec: "pcm_s16be", container: "aiff", outExt: "wav", tf: waxtap.FormatAIFF, want: false},
		{codec: "pcm_s16be", container: "aiff", outExt: "aifc", tf: waxtap.FormatAIFF, want: true},
		// A remux carries the packets into the named container, so a codec
		// the container holds still matches whatever the output is called.
		{codec: "opus", container: "ogg", outExt: "mka", tf: waxtap.FormatOpus, want: true},
	}
	for _, c := range cases {
		p := waxtap.AudioProbe{Codec: c.codec, Container: c.container}
		ext := c.outExt
		if ext == "" {
			ext = transcodeExt(c.tf)
		}
		if got := matchesTargetFamily(p, c.tf, ext); got != c.want {
			t.Errorf("matchesTargetFamily(%q in %q -> .%s, %v) = %v, want %v", c.codec, c.container, ext, c.tf, got, c.want)
		}
	}
}

func TestPlanBatchOutputsSkipsImpossibleProbes(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "a.flac", "b.mp3")
	var probed sync.Map
	probe := func(_ context.Context, path string) (waxtap.AudioProbe, error) {
		probed.Store(filepath.Base(path), true)
		return waxtap.AudioProbe{Codec: map[string]string{"a.flac": "flac", "b.mp3": "mp3"}[filepath.Base(path)], Channels: 2}, nil
	}
	inputs := []string{filepath.Join(root, "a.flac"), filepath.Join(root, "b.mp3")}
	if _, err := planBatchOutputs(context.Background(), inputs, root, filepath.Join(root, "out"), false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded", probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probed.Load("a.flac"); ok {
		t.Error("a.flac (FLAC extension, MP3 target) should not be probed; the extension rules out a match")
	}
	if _, ok := probed.Load("b.mp3"); !ok {
		t.Error("b.mp3 (MP3 extension, MP3 target) should be probed to confirm the no-op")
	}
}

// TestExtPossiblyCodecVideoSpellings covers the video spellings of MP4 and
// Matroska never becoming copy-through candidates. A copy-through delivers the
// whole container, and a probe cannot separate an audio-only .mp4 from a movie,
// so `transcode ./Videos -f aac` used to copy movie.mp4 into the output
// directory untouched.
func TestExtPossiblyCodecVideoSpellings(t *testing.T) {
	for _, ext := range []string{".mp4", ".mkv"} {
		for _, fam := range []string{"aac", "alac", "opus", "vorbis", "flac", "mp3"} {
			if extPossiblyCodec(ext, fam) {
				t.Errorf("extPossiblyCodec(%q,%q) = true; a video container must never be copied through", ext, fam)
			}
		}
	}
	// The audio-only spellings stay copy-eligible so a matching file is still
	// spared a re-encode.
	for _, c := range []struct {
		ext, fam string
		want     bool
	}{
		{".m4a", "aac", true}, {".m4b", "aac", true}, {".m4b", "alac", true},
		{".m4b", "opus", false}, {".mka", "opus", true},
	} {
		if got := extPossiblyCodec(c.ext, c.fam); got != c.want {
			t.Errorf("extPossiblyCodec(%q,%q) = %v, want %v", c.ext, c.fam, got, c.want)
		}
	}
}

// TestAIFFSpellingParity covers the four AIFF spellings across the CLI tables
// that hand-maintain them. IsAIFFExt keeps the media package in step, but a map
// literal cannot call it, so the agreement is asserted here.
func TestAIFFSpellingParity(t *testing.T) {
	for _, sp := range []string{"aiff", "aif", "aifc", "afc"} {
		if !media.IsAIFFExt(sp) {
			t.Fatalf("IsAIFFExt(%q) = false; this test is out of step with the helper", sp)
		}
		if f, err := parseTranscodeFormat(sp); err != nil || f != waxtap.FormatAIFF {
			t.Errorf("parseTranscodeFormat(%q) = %v,%v; want FormatAIFF", sp, f, err)
		}
		if !audioExts["."+sp] {
			t.Errorf("audioExts is missing .%s, so directory processing ignores it", sp)
		}
		if extPossiblyCodec("."+sp, "flac") {
			t.Errorf("extPossiblyCodec(.%s) = true; PCM is not a comparable target family", sp)
		}
		// One output extension for all four input spellings.
		if got := transcodeExt(waxtap.FormatAIFF); got != "aiff" {
			t.Errorf("transcodeExt(FormatAIFF) = %q, want aiff", got)
		}
	}
}

// writeFiles creates fixture files under root.
func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectAudioInputs(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "b.flac", "a.MP3", "notes.txt", "cover.jpg", "sub/c.wav", "sub/d.OPUS", "out/old.mp3")

	t.Run("top level sorted with ignored count", func(t *testing.T) {
		inputs, ignored, err := collectAudioInputs(root, false, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(root, "a.MP3"), filepath.Join(root, "b.flac")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (sorted, case-insensitive ext)", inputs, want)
		}
		if ignored != 2 { // notes.txt, cover.jpg
			t.Errorf("ignored = %d, want 2", ignored)
		}
	})

	t.Run("recursive excludes the output dir", func(t *testing.T) {
		inputs, _, err := collectAudioInputs(root, true, filepath.Join(root, "out"))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range inputs {
			if filepath.Base(in) == "old.mp3" {
				t.Errorf("recursive walk included an output-dir file: %v", inputs)
			}
		}
		// It should find the nested audio files.
		var haveWav, haveOpus bool
		for _, in := range inputs {
			switch filepath.Base(in) {
			case "c.wav":
				haveWav = true
			case "d.OPUS":
				haveOpus = true
			}
		}
		if !haveWav || !haveOpus {
			t.Errorf("recursive inputs missing nested files: %v", inputs)
		}
	})

	// Readable containers must be collected rather than counted as ignored. The
	// AIFF spellings are new; .oga, .mp4, .m4b and .mkv were long-standing gaps,
	// all inferable containers that directory processing skipped.
	t.Run("collects every readable container", func(t *testing.T) {
		dir := t.TempDir()
		readable := []string{
			"a.aiff", "b.aif", "c.aifc", "d.afc",
			"e.oga", "f.mp4", "g.m4b", "h.mkv",
			"i.flac", "j.wav", "k.mp3", "l.m4a", "m.aac",
			"n.opus", "o.ogg", "p.alac", "q.mka", "r.webm",
		}
		writeFiles(t, dir, append(readable, "skip.txt")...)
		inputs, ignored, err := collectAudioInputs(dir, false, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(inputs) != len(readable) {
			t.Errorf("collected %d inputs, want %d: %v", len(inputs), len(readable), inputs)
		}
		if ignored != 1 {
			t.Errorf("ignored = %d, want 1 (skip.txt)", ignored)
		}
	})

	// macOS AppleDouble stubs carry an audio extension but no audio, and would
	// otherwise fail a batch that processed every real file.
	t.Run("skips hidden files", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, "Track.wav", "._Track.wav", ".hidden.flac", ".DS_Store")
		inputs, ignored, err := collectAudioInputs(dir, false, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(dir, "Track.wav")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v", inputs, want)
		}
		if ignored != 3 { // ._Track.wav, .hidden.flac, .DS_Store
			t.Errorf("ignored = %d, want 3 (hidden files counted, not dropped)", ignored)
		}
	})

	t.Run("recursive does not descend hidden directories", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, "Track.wav", ".Trashes/x.mp3", ".Trashes/z.mp3", "sub/y.mp3", "sub/notes.txt")
		inputs, ignored, err := collectAudioInputs(dir, true, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(dir, "Track.wav"), filepath.Join(dir, "sub", "y.mp3")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (.Trashes not descended)", inputs, want)
		}
		// A skipped directory's contents are not walked, so they are not counted;
		// only notes.txt is.
		if ignored != 1 {
			t.Errorf("ignored = %d, want 1 (.Trashes contents are never visited)", ignored)
		}
	})

	// Naming a hidden directory still processes it: only entries below the root
	// are skipped.
	t.Run("recursive hidden root still walks", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, ".music/a.mp3", ".music/nested/b.mp3")
		root := filepath.Join(dir, ".music")
		inputs, _, err := collectAudioInputs(root, true, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(root, "a.mp3"), filepath.Join(root, "nested", "b.mp3")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (a hidden root the user named still walks)", inputs, want)
		}
	})
}

// symlinkOrSkip links name to target, skipping the test where the platform will
// not make one: Windows creates a symlink only in developer mode or with
// elevation. The skip is a runtime check rather than a build-tagged file because
// the behavior under test is not unix-only, only the fixture is.
func symlinkOrSkip(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

// A symlinked file failed the regular-file test before any classifier ran, so it
// was neither processed nor counted anywhere in the summary: it vanished. A
// single named input has always been stat'd (isLocalFile), so directory mode was
// the inconsistent one.
func TestCollectAudioInputsSymlinks(t *testing.T) {
	t.Run("link to an audio file is collected", func(t *testing.T) {
		store := t.TempDir()
		writeFiles(t, store, "album.flac", "notes.txt")
		dir := t.TempDir()
		writeFiles(t, dir, "real.mp3")
		symlinkOrSkip(t, filepath.Join(store, "album.flac"), filepath.Join(dir, "link.flac"))
		symlinkOrSkip(t, filepath.Join(store, "notes.txt"), filepath.Join(dir, "link.txt"))

		inputs, ignored, err := collectAudioInputs(dir, false, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(dir, "link.flac"), filepath.Join(dir, "real.mp3")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (a link is classified by its own extension)", inputs, want)
		}
		if ignored != 1 { // link.txt
			t.Errorf("ignored = %d, want 1 (the linked .txt)", ignored)
		}
	})

	t.Run("broken link counts as ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, "real.mp3")
		symlinkOrSkip(t, filepath.Join(dir, "gone.flac"), filepath.Join(dir, "dangling.flac"))

		inputs, ignored, err := collectAudioInputs(dir, false, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(dir, "real.mp3")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (a broken link is not an input)", inputs, want)
		}
		if ignored != 1 {
			t.Errorf("ignored = %d, want 1 (the broken link is counted, not dropped)", ignored)
		}
	})

	// The link carries an audio extension, so treating it as a regular file would
	// schedule a directory for encoding.
	t.Run("link to a directory counts as ignored", func(t *testing.T) {
		store := t.TempDir()
		writeFiles(t, store, "inside.mp3")
		dir := t.TempDir()
		writeFiles(t, dir, "real.mp3")
		symlinkOrSkip(t, store, filepath.Join(dir, "linked.mp3"))

		for _, recursive := range []bool{false, true} {
			inputs, ignored, err := collectAudioInputs(dir, recursive, "")
			if err != nil {
				t.Fatal(err)
			}
			want := []string{filepath.Join(dir, "real.mp3")}
			if !reflect.DeepEqual(inputs, want) {
				t.Errorf("recursive=%v inputs = %v, want %v (a linked directory is neither an input nor descended)", recursive, inputs, want)
			}
			if ignored != 1 {
				t.Errorf("recursive=%v ignored = %d, want 1 (the link itself)", recursive, ignored)
			}
		}
	})
}

// The excluded output directory was compared by absolute spelling alone, so
// --dir naming it through a link left the walk treating the run's own output as
// input. Resolution has to stay additive: --dir usually names a directory the
// run has not created yet, and EvalSymlinks fails on a path that does not exist.
func TestCollectAudioInputsExcludeDir(t *testing.T) {
	t.Run("matches through a link", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "keep.mp3", "out/old.mp3")
		link := filepath.Join(t.TempDir(), "out-link")
		symlinkOrSkip(t, filepath.Join(root, "out"), link)

		inputs, _, err := collectAudioInputs(root, true, link)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(root, "keep.mp3")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (the output directory was named through a link)", inputs, want)
		}
	})

	t.Run("a directory that does not exist yet excludes nothing", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "keep.mp3", "sub/deep.mp3")

		inputs, _, err := collectAudioInputs(root, true, filepath.Join(root, "out"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(root, "keep.mp3"), filepath.Join(root, "sub", "deep.mp3")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (an unresolvable --dir must not match every directory)", inputs, want)
		}
	})

	// --dir equal to the root is not an exclusion, or the run would skip
	// everything. That still has to hold when the two are spelled differently.
	t.Run("the root itself is never the exclusion", func(t *testing.T) {
		target := t.TempDir()
		writeFiles(t, target, "keep.mp3")
		link := filepath.Join(t.TempDir(), "root-link")
		symlinkOrSkip(t, target, link)

		inputs, _, err := collectAudioInputs(link, false, target)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(link, "keep.mp3")}
		if !reflect.DeepEqual(inputs, want) {
			t.Errorf("inputs = %v, want %v (--dir naming the root through a link is not an exclusion)", inputs, want)
		}
	})
}

// stubProbe returns codecs keyed by file basename, reported as stereo: these
// cases are about the codec, not the layout.
func stubProbe(codecs map[string]string) func(context.Context, string) (waxtap.AudioProbe, error) {
	return func(_ context.Context, path string) (waxtap.AudioProbe, error) {
		if c, ok := codecs[filepath.Base(path)]; ok {
			return waxtap.AudioProbe{Codec: c, Channels: 2}, nil
		}
		return waxtap.AudioProbe{}, errors.New("no codec")
	}
}

func TestPlanBatchOutputs(t *testing.T) {
	ctx := context.Background()

	t.Run("rejects copy", func(t *testing.T) {
		_, err := planBatchOutputs(ctx, []string{"a.flac"}, ".", "out", false, waxtap.FormatCopy, waxtap.ProcessSpec{}, collisionFail, false, "transcoded", stubProbe(nil))
		if _, ok := errors.AsType[*usageError](err); !ok {
			t.Errorf("copy err = %v, want usageError", err)
		}
	})

	t.Run("no-op copy into dir and unchanged in place", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "song.mp3")
		in := filepath.Join(root, "song.mp3")
		probe := stubProbe(map[string]string{"song.mp3": "mp3"})

		// A matching codec is copied unchanged into --dir.
		jobs, err := planBatchOutputs(ctx, []string{in}, root, filepath.Join(root, "out"), false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded", probe)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].action != actCopy {
			t.Fatalf("jobs = %+v, want one actCopy", jobs)
		}
		if filepath.Base(jobs[0].output) != "song.mp3" {
			t.Errorf("copy output = %q, want it to preserve the source name", jobs[0].output)
		}

		// Without --dir, a matching codec remains in place.
		jobs, err = planBatchOutputs(ctx, []string{in}, root, "", false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded", probe)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].action != actUnchanged {
			t.Fatalf("jobs = %+v, want one actUnchanged", jobs)
		}
	})

	t.Run("no-op into the input's own dir is unchanged, not a self-overwrite", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "song.mp3", "other.flac")
		inputs := []string{filepath.Join(root, "other.flac"), filepath.Join(root, "song.mp3")}
		// With --dir equal to root, the MP3 output resolves to the input path.
		jobs, err := planBatchOutputs(ctx, inputs, root, root, false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded",
			stubProbe(map[string]string{"song.mp3": "mp3", "other.flac": "flac"}))
		if err != nil {
			t.Fatalf("planBatchOutputs aborted instead of leaving the no-op unchanged: %v", err)
		}
		var unchanged, process int
		for _, j := range jobs {
			switch j.action {
			case actUnchanged:
				unchanged++
			case actProcess:
				process++
			}
		}
		if unchanged != 1 || process != 1 {
			t.Errorf("jobs = %+v, want 1 unchanged (song.mp3) + 1 process (other.flac)", jobs)
		}
	})

	t.Run("force re-encodes a would-be no-op", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "song.mp3")
		in := filepath.Join(root, "song.mp3")
		jobs, err := planBatchOutputs(ctx, []string{in}, root, filepath.Join(root, "out"), false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, true, "transcoded", stubProbe(map[string]string{"song.mp3": "mp3"}))
		if err != nil {
			t.Fatal(err)
		}
		if jobs[0].action != actProcess {
			t.Errorf("forced job action = %v, want actProcess", jobs[0].action)
		}
	})

	t.Run("rejects two inputs mapping to one output", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "song.wav", "song.flac")
		inputs := []string{filepath.Join(root, "song.flac"), filepath.Join(root, "song.wav")}
		// Neither codec matches the target, and both outputs map to song.mp3.
		_, err := planBatchOutputs(ctx, inputs, root, filepath.Join(root, "out"), false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded", stubProbe(map[string]string{"song.flac": "flac", "song.wav": "pcm_s16le"}))
		if _, ok := errors.AsType[*usageError](err); !ok {
			t.Errorf("clobber err = %v, want usageError (song.wav+song.flac -> song.mp3)", err)
		}
	})

	t.Run("rejects output equal to an input", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "a.flac", "a.mp3")
		// a.flac -> a.mp3 (in --dir == root) collides with the existing a.mp3 input.
		inputs := []string{filepath.Join(root, "a.flac"), filepath.Join(root, "a.mp3")}
		_, err := planBatchOutputs(ctx, inputs, root, root, false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded", stubProbe(map[string]string{"a.flac": "flac", "a.mp3": "opus"}))
		if _, ok := errors.AsType[*usageError](err); !ok {
			t.Errorf("output==input err = %v, want usageError", err)
		}
	})
}

func TestRunBatchJobs(t *testing.T) {
	jobs := []batchJob{
		{index: 0, input: "ok.flac", output: "out/ok.mp3", action: actProcess},
		{index: 1, input: "bad.flac", output: "out/bad.mp3", action: actProcess},
		{index: 2, input: "skip.flac", output: "out/skip.mp3", action: actSkip},
		{index: 3, input: "same.mp3", output: "same.mp3", action: actUnchanged},
	}
	processFn := func(_ context.Context, input, output string) (*waxtap.Result, error) {
		if input == "bad.flac" {
			return nil, waxtap.ErrUnsupportedInput
		}
		return &waxtap.Result{OutputPath: output}, nil
	}
	outcomes := runBatchJobs(context.Background(), jobs, 2, processFn, nil)
	if len(outcomes) != 4 {
		t.Fatalf("outcomes = %d, want 4", len(outcomes))
	}
	wantStatus := []batchStatus{statusOK, statusError, statusSkipped, statusUnchanged}
	for i, w := range wantStatus {
		if outcomes[i].status != w {
			t.Errorf("outcome[%d].status = %v, want %v", i, outcomes[i].status, w)
		}
	}
	if !errors.Is(outcomes[1].err, waxtap.ErrUnsupportedInput) {
		t.Errorf("failed outcome err = %v, want ErrUnsupportedInput (continue-on-error)", outcomes[1].err)
	}
}

func TestRunBatchJobsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before scheduling
	jobs := []batchJob{{index: 0, input: "a.flac", output: "a.mp3", action: actProcess}}
	called := false
	outcomes := runBatchJobs(ctx, jobs, 1, func(context.Context, string, string) (*waxtap.Result, error) {
		called = true
		return &waxtap.Result{}, nil
	}, nil)
	if called {
		t.Error("processFn should not run after cancellation")
	}
	if outcomes[0].status != statusNotRun {
		t.Errorf("status = %v, want not-run on cancellation", outcomes[0].status)
	}
}

func TestRepresentativeError(t *testing.T) {
	pathErr := &fs.PathError{Op: "open", Path: "/x", Err: errors.New("boom")} // exit 10
	outcomes := []batchOutcome{
		{err: waxtap.ErrUnsupportedInput}, // exit 2
		{err: waxtap.ErrRateLimited},      // exit 5
		{err: pathErr},                    // exit 10 (most serious)
		{status: statusOK},                // no error
	}
	if rep := representativeError(outcomes); rep != error(pathErr) {
		t.Errorf("representative = %v, want the exit-10 path error (highest)", rep)
	}
	if representativeError(nil) != nil {
		t.Error("representativeError(nil) should be nil")
	}
}

func TestBatchConcurrency(t *testing.T) {
	env := testResolveEnv()
	t.Run("zero means serial", func(t *testing.T) {
		got, err := batchConcurrency(env, 0)
		if err != nil || got != 1 {
			t.Errorf("batchConcurrency(0) = %d, %v; want 1, nil", got, err)
		}
	})
	t.Run("negative rejected", func(t *testing.T) {
		if _, err := batchConcurrency(env, -1); err == nil {
			t.Error("batchConcurrency(-1) should error")
		}
	})
	t.Run("clamped above the ceiling", func(t *testing.T) {
		got, err := batchConcurrency(env, maxConcurrency+100)
		if err != nil || got != maxConcurrency {
			t.Errorf("batchConcurrency(over) = %d, %v; want %d, nil", got, err, maxConcurrency)
		}
	})
}

// copyThrough is the one batch write that bypassed outputFor, so --collision
// fail could be silently last-writer-wins on a copied file and auto-number
// could not renumber a publish race.
func TestCopyThroughHonorsCollisionMode(t *testing.T) {
	stage := func(t *testing.T) (string, string) {
		t.Helper()
		dir := t.TempDir()
		src := filepath.Join(dir, "src.mp3")
		if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, "out.mp3")
		if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		return src, dst
	}

	t.Run("fail refuses an occupied path", func(t *testing.T) {
		src, dst := stage(t)
		if _, err := copyThrough(src, dst, collisionFail); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("err = %v, want fs.ErrExist", err)
		}
		if b, _ := os.ReadFile(dst); string(b) != "old" {
			t.Errorf("dst = %q; the occupying file must be intact", b)
		}
	})

	t.Run("auto-number renumbers", func(t *testing.T) {
		src, dst := stage(t)
		got, err := copyThrough(src, dst, collisionAutoNumber)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(got) != "out (1).mp3" {
			t.Errorf("published %q, want %q", filepath.Base(got), "out (1).mp3")
		}
		if b, _ := os.ReadFile(dst); string(b) != "old" {
			t.Errorf("dst = %q; the occupying file must be intact", b)
		}
	})

	t.Run("overwrite replaces", func(t *testing.T) {
		src, dst := stage(t)
		got, err := copyThrough(src, dst, collisionOverwrite)
		if err != nil || got != dst {
			t.Fatalf("copyThrough = %q, %v; want the destination replaced", got, err)
		}
		if b, _ := os.ReadFile(dst); string(b) != "new" {
			t.Errorf("dst = %q, want the copy delivered", b)
		}
	})
}

// The rendered outcome must name the file the run actually wrote: under
// auto-number a publish can renumber past the pre-flight pick, for processed
// and copied items both.
func TestRunBatchJobsReportsPublishedPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mp3")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(dir, "copy.mp3")
	if err := os.WriteFile(occupied, []byte("taken"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := []batchJob{
		{index: 0, input: src, output: filepath.Join(dir, "a.mp3"), action: actProcess},
		{index: 1, input: src, output: occupied, action: actCopy, mode: collisionAutoNumber},
	}
	renumbered := filepath.Join(dir, "a (1).mp3")
	processFn := func(_ context.Context, _, _ string) (*waxtap.Result, error) {
		return &waxtap.Result{OutputPath: renumbered}, nil
	}
	outcomes := runBatchJobs(context.Background(), jobs, 1, processFn, nil)
	if outcomes[0].output != renumbered {
		t.Errorf("processed outcome path = %q, want the result's %q", outcomes[0].output, renumbered)
	}
	if want := filepath.Join(dir, "copy (1).mp3"); outcomes[1].output != want {
		t.Errorf("copied outcome path = %q, want %q", outcomes[1].output, want)
	}
}

// A recursive walk of a root that is itself a symlink to a directory used to
// find nothing at all: filepath.WalkDir Lstats the final component of its root
// and does not follow a link there, so the walk visited exactly one entry (the
// link) and counted it ignored. The shallow path reads the same root through
// os.ReadDir, which follows the link, so `transcode <link>` worked and
// `transcode <link> -r` reported "no recognized audio files found" over a
// library of thousands.
func TestCollectAudioInputsSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeFiles(t, real, "a.flac", "album/b.flac", "album/notes.txt")
	link := filepath.Join(base, "link")
	symlinkOrSkip(t, real, link)

	t.Run("recursive walk follows the root link", func(t *testing.T) {
		inputs, ignored, err := collectAudioInputs(link, true, "")
		if err != nil {
			t.Fatal(err)
		}
		// Spelled under the argument, exactly as the shallow path spells them:
		// downstream planning mirrors --dir layouts with literal prefix math
		// (relUnder), and the summary should name paths the way the user did.
		want := []string{filepath.Join(link, "a.flac"), filepath.Join(link, "album", "b.flac")}
		if !slices.Equal(inputs, want) {
			t.Errorf("inputs = %v, want %v", inputs, want)
		}
		if ignored != 1 {
			t.Errorf("ignored = %d, want 1 (notes.txt)", ignored)
		}
	})

	t.Run("matches a walk of the real root", func(t *testing.T) {
		viaLink, _, err := collectAudioInputs(link, true, "")
		if err != nil {
			t.Fatal(err)
		}
		viaReal, _, err := collectAudioInputs(real, true, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(viaLink) != len(viaReal) {
			t.Fatalf("link walk found %d files, real walk %d; the two roots name one directory", len(viaLink), len(viaReal))
		}
	})

	t.Run("dir exclusion still holds under a linked root", func(t *testing.T) {
		out := filepath.Join(real, "out")
		writeFiles(t, out, "done.flac")
		inputs, _, err := collectAudioInputs(link, true, filepath.Join(link, "out"))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range inputs {
			if strings.Contains(in, "done.flac") {
				t.Errorf("the excluded output dir was walked: %v", inputs)
			}
		}
	})
}

// The planner's guards compare filepath.Abs spellings, so a --dir that reaches
// an input's directory through a symlink used to evade all three: the
// overwrite-an-input rejection fell through to a misleading collision error,
// the unchanged-in-place detection planned a self-copy that rewrote the input
// under --collision overwrite, and two spellings of one output were never seen
// as a duplicate.
func TestPlanBatchOutputsSeesThroughLinks(t *testing.T) {
	ctx := context.Background()

	t.Run("overwrite-an-input guard", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "src")
		writeFiles(t, root, "song.mp3")
		link := filepath.Join(base, "dirlink")
		symlinkOrSkip(t, root, link)

		// force makes the mp3->mp3 mapping a real re-encode, so the planned
		// output is the input file itself, reached through the link.
		_, err := planBatchOutputs(ctx, []string{filepath.Join(root, "song.mp3")}, root, link, false,
			waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, true, "transcoded",
			stubProbe(map[string]string{"song.mp3": "mp3"}))
		if err == nil {
			t.Fatal("a re-encode onto its own input through a link was planned")
		}
		if !strings.Contains(err.Error(), "overwrite an input") {
			t.Errorf("err = %v, want the overwrite-an-input rejection, not a collision message", err)
		}
	})

	t.Run("unchanged in place through a link", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "src")
		writeFiles(t, root, "song.mp3")
		link := filepath.Join(base, "dirlink")
		symlinkOrSkip(t, root, link)

		jobs, err := planBatchOutputs(ctx, []string{filepath.Join(root, "song.mp3")}, root, link, false,
			waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded",
			stubProbe(map[string]string{"song.mp3": "mp3"}))
		if err != nil {
			t.Fatalf("planBatchOutputs = %v; a no-op mapped onto itself is unchanged, not a collision", err)
		}
		if len(jobs) != 1 || jobs[0].action != actUnchanged {
			t.Fatalf("jobs = %+v, want one actUnchanged; a self-copy rewrites the input it reads", jobs)
		}
	})

	t.Run("duplicate outputs across two spellings", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "src")
		writeFiles(t, root, "song.wav", "song.flac")
		link := filepath.Join(base, "dirlink")
		symlinkOrSkip(t, root, link)

		// In place (no --dir), both re-encode to song.mp3 beside themselves: one
		// physical destination spelled two ways.
		inputs := []string{filepath.Join(root, "song.wav"), filepath.Join(link, "song.flac")}
		_, err := planBatchOutputs(ctx, inputs, root, "", false,
			waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false, "transcoded", stubProbe(nil))
		if err == nil {
			t.Fatal("two inputs mapping to one physical output were both planned")
		}
		if !strings.Contains(err.Error(), "both map to output") {
			t.Errorf("err = %v, want the duplicate-output rejection", err)
		}
	})

	t.Run("a --dir that does not exist yet still plans", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, "song.wav")
		jobs, err := planBatchOutputs(ctx, []string{filepath.Join(root, "song.wav")}, root,
			filepath.Join(root, "not-yet"), false, waxtap.FormatMP3, waxtap.ProcessSpec{}, collisionFail, false,
			"transcoded", stubProbe(nil))
		if err != nil {
			t.Fatalf("planBatchOutputs = %v; resolution must fall back for a directory the run will create", err)
		}
		if len(jobs) != 1 || jobs[0].action != actProcess {
			t.Fatalf("jobs = %+v, want one actProcess", jobs)
		}
	})
}

// TestAudioExtsDecodeOnlyInputs: a directory walk claims the decode-only
// formats by their conventional spelling, since a file it can transcode is a
// file it should process, and leaves the loose spellings (.asf for WMA, .mp+
// and .mpp for Musepack) to be named directly, like the other loose spellings
// audioExts documents.
func TestAudioExtsDecodeOnlyInputs(t *testing.T) {
	for _, ext := range []string{".wma", ".mpc"} {
		if !audioExts[ext] {
			t.Errorf("audioExts[%q] = false; directory processing would ignore a decodable input", ext)
		}
	}
	for _, ext := range []string{".asf", ".mp+", ".mpp"} {
		if audioExts[ext] {
			t.Errorf("audioExts[%q] = true; the loose spellings are named directly, not walked", ext)
		}
	}
}
