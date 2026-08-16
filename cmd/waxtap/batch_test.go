package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
	}
	for _, c := range cases {
		if got := extPossiblyCodec(c.ext, c.family); got != c.want {
			t.Errorf("extPossiblyCodec(%q,%q) = %v, want %v", c.ext, c.family, got, c.want)
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
