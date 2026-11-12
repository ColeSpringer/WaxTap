package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
)

// TestAlbumTableHeaders guards the album table labels. Process output uses
// IN-LUFS for per-track input loudness because the row also includes OUTPUT;
// measure output has no output column and keeps LUFS.
func TestAlbumTableHeaders(t *testing.T) {
	inputs := []string{"a.flac", "b.flac"}
	perTrack := []waxtap.LoudnessInfo{{IntegratedLUFS: -11}, {IntegratedLUFS: -13}}

	t.Run("process table uses IN-LUFS", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		res := &waxtap.AlbumProcessResult{
			Album:    waxtap.LoudnessInfo{IntegratedLUFS: -12},
			GainDB:   -2,
			PerTrack: perTrack,
			Outputs:  []string{"out/a.flac", "out/b.flac"},
			// One track carried its chapters, the other had nothing to carry.
			TagCarry: []*waxtap.TagCarry{
				{Items: []waxtap.CarryItem{{Kind: waxtap.CarryChapters, Count: 3, Disposition: waxtap.DispositionCarried}}},
				nil,
			},
		}
		if err := emitAlbumProcess(env, inputs, res); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		if !strings.Contains(s, "IN-LUFS") || !strings.Contains(s, "METADATA") || !strings.Contains(s, "OUTPUT") {
			t.Errorf("process table header = %q, want IN-LUFS, METADATA, and OUTPUT", s)
		}
		// The receipt column is the single-file Metadata: line per track, "-"
		// where no carry ran.
		if !strings.Contains(s, "3 chapters carried") {
			t.Errorf("process table = %q, want track 1's receipt", s)
		}
		if !strings.Contains(s, "\t-\t") && !strings.Contains(s, "  -  ") {
			t.Errorf("process table = %q, want a - cell for the track that carried nothing", s)
		}
	})

	t.Run("measure table keeps LUFS", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		res := &waxtap.AlbumLoudnessResult{
			Album:    waxtap.LoudnessInfo{IntegratedLUFS: -12},
			PerTrack: perTrack,
		}
		if err := emitAlbumMeasure(env, inputs, res); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		// Locate the row naming TRACK and require the plain LUFS column.
		var header string
		for line := range strings.SplitSeq(s, "\n") {
			if strings.Contains(line, "TRACK") {
				header = line
				break
			}
		}
		if header == "" {
			t.Fatalf("measure table has no TRACK header line: %q", s)
		}
		if !strings.Contains(header, "LUFS") || strings.Contains(header, "IN-LUFS") {
			t.Errorf("measure table header = %q, want a plain LUFS column", header)
		}
	})
}

func TestNormalizeFlagSurface(t *testing.T) {
	flags := newNormalizeCmd().Flags()
	for _, name := range []string{"measure-loudness", "loudness-target", "peak-mode", "format", "bitrate", "collision"} {
		if flags.Lookup(name) == nil {
			t.Errorf("normalize should expose --%s", name)
		}
	}
	// Normalize writes by default; --measure-loudness selects analysis-only mode.
	for _, name := range []string{"apply", "normalize", "measure"} {
		if flags.Lookup(name) != nil {
			t.Errorf("normalize should not expose --%s", name)
		}
	}
}

// TestNormalizeMeasureRejectsWriteFlags verifies that analysis-only mode rejects
// output-related flags and a positional output path.
func TestNormalizeMeasureRejectsWriteFlags(t *testing.T) {
	for _, args := range [][]string{
		{"in.wav", "--measure-loudness", "--loudness-target", "-16"},
		{"in.wav", "--measure-loudness", "--peak-mode", "limit"},
		{"in.wav", "--measure-loudness", "--format", "flac"},
		{"in.wav", "--measure-loudness", "--bitrate", "128000"},
		{"in.wav", "--measure-loudness", "--out", "out.flac"},
		{"in.wav", "--measure-loudness", "--dir", "out"},
		{"in.wav", "--measure-loudness", "--collision", "overwrite"},
		{"in.wav", "--measure-loudness", "--channels", "mono"},
		{"in.wav", "--measure-loudness", "--downmix"},
		{"in.wav", "out.flac", "--measure-loudness"}, // positional output
	} {
		assertNormalizeUsageError(t, args)
	}
}

// TestNormalizeWriteByDefaultNeedsOutput checks every supported input shape.
func TestNormalizeWriteByDefaultNeedsOutput(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.wav")
	f2 := filepath.Join(dir, "b.wav")
	for _, p := range []string{f1, f2} {
		if err := os.WriteFile(p, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	emptyDir := t.TempDir()

	cases := []struct {
		name string
		args []string
	}{
		// An existing file with no output/--format: the "needs output" hint fires.
		// A missing file is intercepted earlier by source validation and reported as
		// "no such file", covered by TestProcessSourceCheckedBeforeCollision.
		{"file", []string{f1}},
		{"album", []string{"--album", f1, f2}},
		{"directory", []string{emptyDir}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newNormalizeCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Fatalf("normalize %v: err = %v (%T), want *usageError", tc.args, err, err)
			}
			if !strings.Contains(err.Error(), "--measure-loudness") {
				t.Errorf("normalize %v: message = %q, want it to name --measure-loudness", tc.args, err)
			}
		})
	}
}

// TestNormalizeAlbumEmptyFlagsKeepAlbumMessage verifies that an explicitly empty
// --dir/--out in album mode keeps album's specific requirement errors rather than
// the generic empty-path hint. --album requires --dir and rejects --out, so "omit
// it to use the default location" would be misleading.
func TestNormalizeAlbumEmptyFlagsKeepAlbumMessage(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.wav")
	f2 := filepath.Join(dir, "b.wav")
	for _, p := range []string{f1, f2} {
		if err := os.WriteFile(p, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty --dir", []string{"--album", "--format", "flac", "--dir", "", f1, f2}, "pass --dir"},
		{"empty --out", []string{"--album", "--format", "flac", "--out", "", f1, f2}, "not used with --album"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newNormalizeCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Fatalf("normalize %v: err = %v (%T), want *usageError", tc.args, err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("normalize %v: message = %q, want album-specific %q", tc.args, err, tc.want)
			}
			if strings.Contains(err.Error(), "omit it to use the default location") {
				t.Errorf("normalize %v: message = %q, leaked the generic empty-path hint", tc.args, err)
			}
		})
	}
}

// TestValidateNormalizeModeFlags checks flags whose validity depends on mode.
func TestValidateNormalizeModeFlags(t *testing.T) {
	writeFlags := []struct{ name, val string }{
		{"loudness-target", "-16"}, {"format", "flac"}, {"bitrate", "128000"},
		{"out", "o.flac"}, {"dir", "d"}, {"collision", "overwrite"},
		{"channels", "mono"}, {"downmix", "true"},
	}
	for _, wf := range writeFlags {
		write := newNormalizeCmd()
		mustSet(t, write, wf.name, wf.val)
		if err := validateNormalizeModeFlags(write, false); err != nil {
			t.Errorf("write mode rejected --%s: %v", wf.name, err)
		}
		measure := newNormalizeCmd()
		mustSet(t, measure, wf.name, wf.val)
		if err := validateNormalizeModeFlags(measure, true); err == nil {
			t.Errorf("--measure-loudness accepted --%s, want a usage error", wf.name)
		}
	}
}

// TestValidateNormalizeInputFlags checks flags whose validity depends on input
// shape.
func TestValidateNormalizeInputFlags(t *testing.T) {
	cases := []struct {
		name              string
		measure, dir, alb bool
		flag, val         string
		wantErr           bool
	}{
		// Single-file writes reject the directory-batch flags, including --dir.
		{"single write recursive", false, false, false, "recursive", "true", true},
		{"single write concurrency", false, false, false, "concurrency", "2", true},
		{"single write dir", false, false, false, "dir", "out", true},
		// Single-file measurement rejects recursive/concurrency (--dir is already
		// rejected by the mode check).
		{"single measure recursive", true, false, false, "recursive", "true", true},
		{"single measure concurrency", true, false, false, "concurrency", "2", true},
		// Directory writes accept batch flags but reject URL-only flags.
		{"dir write recursive ok", false, true, false, "recursive", "true", false},
		{"dir write concurrency ok", false, true, false, "concurrency", "2", false},
		{"dir write itag", false, true, false, "itag", "140", true},
		{"dir write codec", false, true, false, "codec", "opus", true},
		// Albums reject URL flags and per-file/channel flags.
		{"album itag", false, false, true, "itag", "140", true},
		{"album recursive", false, false, true, "recursive", "true", true},
		{"album channels", false, false, true, "channels", "mono", true},
		{"album out", false, false, true, "out", "o.flac", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newNormalizeCmd()
			mustSet(t, cmd, tc.flag, tc.val)
			err := validateNormalizeInputFlags(cmd, tc.measure, tc.dir, tc.alb)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateNormalizeInputFlags err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestNormalizeAlbumPeakMode: album takes both peak modes. cap used to be
// refused, on the reasoning that a per-track clamp destroys the spacing album
// mode preserves; the clamp is album-wide, so it does the opposite, and refusing
// it left the mandatory limiter compressing that spacing with no way out.
func TestNormalizeAlbumPeakMode(t *testing.T) {
	dir := t.TempDir()
	var inputs []string
	for _, n := range []string{"a.flac", "b.flac"} {
		p := filepath.Join(dir, n)
		synthAudio(t, p, "flac")
		inputs = append(inputs, p)
	}

	// The empty mode is the case the gate exists for: --album defaults to limit
	// whatever --peak-mode's own default is, and only an explicit flag changes it.
	for _, mode := range []string{"", "cap", "limit"} {
		t.Run("mode="+mode, func(t *testing.T) {
			out := filepath.Join(dir, "out-"+mode)
			args := []string{"--album", "--format", "flac", "--dir", out}
			if mode != "" {
				args = append(args, "--peak-mode", mode)
			}
			args = append(args, inputs...)
			cmd := newNormalizeCmd()
			cmd.SetArgs(args)
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			if err := cmd.Execute(); err != nil {
				t.Errorf("--album --peak-mode %s = %v\n%s", mode, err, buf.String())
			}
		})
	}
}

func assertNormalizeUsageError(t *testing.T, args []string) {
	t.Helper()
	cmd := newNormalizeCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("normalize %v: err = %v (%T), want *usageError (exit 2)", args, err, err)
	}
}

// The warning above an album or split table folds every damaged row into one
// line that names the first, so the table says what each of the others lost:
// a MISSING column, in seconds, present only when some row lost audio. A run
// with nothing missing prints the table it always did.
func TestTablesShowWhatEachRowLost(t *testing.T) {
	inputs := []string{"a.flac", "b.flac", "c.flac"}
	per := []waxtap.LoudnessInfo{{IntegratedLUFS: -11}, {IntegratedLUFS: -12}, {IntegratedLUFS: -13}}
	outs := []string{"out/a.flac", "out/b.flac", "out/c.flac"}
	losses := []*waxtap.InputLoss{
		nil,
		{TruncatedSamples: 8820, SampleRate: 44100},                     // 0.20 s
		{LostSamples: 4410, TruncatedSamples: 13230, SampleRate: 44100}, // 0.10 s and 0.30 s
	}
	render := func(emit func(env *appEnv) error) string {
		t.Helper()
		var out bytes.Buffer
		if err := emit(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	tables := map[string]func(losses []*waxtap.InputLoss) string{
		"album process": func(l []*waxtap.InputLoss) string {
			return render(func(env *appEnv) error {
				return emitAlbumProcess(env, inputs, &waxtap.AlbumProcessResult{PerTrack: per, Outputs: outs, TagCarry: make([]*waxtap.TagCarry, 3), InputLoss: l})
			})
		},
		"album measure": func(l []*waxtap.InputLoss) string {
			return render(func(env *appEnv) error {
				return emitAlbumMeasure(env, inputs, &waxtap.AlbumLoudnessResult{PerTrack: per, InputLoss: l})
			})
		},
		"split": func(l []*waxtap.InputLoss) string {
			plan := &waxtap.SplitPlan{Input: "rip.flac", TrackTotal: 3, Pieces: []waxtap.SplitPiece{{Track: 1}, {Track: 2}, {Track: 3}}}
			return render(func(env *appEnv) error {
				return emitSplit(env, plan, &waxtap.SplitResult{Outputs: outs, TagCarry: make([]*waxtap.TagCarry, 3), InputLoss: l}, "rip.cue")
			})
		},
	}
	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			s := table(losses)
			for _, want := range []string{"MISSING", "0.20 s off the end", "0.10 s as silence, 0.30 s off the end"} {
				if !strings.Contains(s, want) {
					t.Errorf("table = %q, want %q", s, want)
				}
			}
			// The whole row's cell is a dash, between its neighbours.
			var first string
			for line := range strings.SplitSeq(s, "\n") {
				if strings.HasPrefix(line, "1 ") {
					first = line
				}
			}
			if !strings.Contains(first, " - ") {
				t.Errorf("row 1 = %q, want a - cell for the row that lost nothing", first)
			}
			for _, clean := range [][]*waxtap.InputLoss{nil, {nil, nil, nil}} {
				if s := table(clean); strings.Contains(s, "MISSING") {
					t.Errorf("a run that lost nothing prints %q, want no MISSING column", s)
				}
			}
		})
	}

	// Counts with no rate to time them at are stated as the samples they are.
	if got := lossCell([]*waxtap.InputLoss{{LostSamples: 1, TruncatedSamples: 2048}}, 0); got != "1 sample as silence, 2048 samples off the end" {
		t.Errorf("lossCell with no rate = %q", got)
	}
	if got := lossCell(nil, 4); got != "-" {
		t.Errorf("lossCell past the list = %q, want -", got)
	}
}

// TestAlbumTracksJSONTagCarry pins the per-track carry in the album document:
// present with the report, absent for a track that carried nothing and for a
// measurement, which carries nothing at all.
func TestAlbumTracksJSONTagCarry(t *testing.T) {
	inputs := []string{"a.flac", "b.flac"}
	per := []waxtap.LoudnessInfo{{}, {}}
	carries := []*waxtap.TagCarry{
		{Items: []waxtap.CarryItem{{Kind: waxtap.CarryChapters, Count: 3, Disposition: waxtap.DispositionCarried}}},
		nil,
	}
	rows := albumTracksJSON(inputs, per, []string{"out/a.flac", "out/b.flac"}, carries, nil)
	if rows[0].TagCarry == nil || len(rows[0].TagCarry.Items) != 1 || rows[0].TagCarry.Items[0].Kind != "chapters" {
		t.Errorf("track 0 tagCarry = %+v, want the chapter item", rows[0].TagCarry)
	}
	if rows[1].TagCarry != nil {
		t.Errorf("track 1 tagCarry = %+v, want none for a track that carried nothing", rows[1].TagCarry)
	}
	for i, row := range albumTracksJSON(inputs, per, nil, nil, nil) {
		if row.TagCarry != nil || row.Output != "" {
			t.Errorf("measurement row %d = %+v, want no output and no carry", i, row)
		}
	}
}

// TestAlbumTracksJSONInputLoss pins the per-track count in the album
// document: the three numbers on the track that lost audio, and no key at all
// on one that lost nothing, for a measurement and a write alike.
func TestAlbumTracksJSONInputLoss(t *testing.T) {
	inputs := []string{"a.flac", "b.flac"}
	per := []waxtap.LoudnessInfo{{}, {}}
	losses := []*waxtap.InputLoss{nil, {TruncatedSamples: 8192, SampleRate: 44100}}
	for name, rows := range map[string][]albumTrackJSON{
		"measure": albumTracksJSON(inputs, per, nil, nil, losses),
		"process": albumTracksJSON(inputs, per, []string{"out/a.flac", "out/b.flac"}, []*waxtap.TagCarry{nil, nil}, losses),
	} {
		raw, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got[0]["inputLoss"]; ok {
			t.Errorf("%s: track 0 = %v, want no inputLoss key for a track that lost nothing", name, got[0])
		}
		loss, _ := got[1]["inputLoss"].(map[string]any)
		if loss["lostSamples"] != float64(0) || loss["truncatedSamples"] != float64(8192) || loss["sampleRate"] != float64(44100) {
			t.Errorf("%s: track 1 inputLoss = %v, want all three numbers present", name, got[1]["inputLoss"])
		}
	}
}

// An album --dir that names a file, and two inputs whose names collide in the
// output directory, are both request errors: the first used to fail per track
// as an I/O error, the second wrote one track over another.
func TestNormalizeAlbumPreflightRejectsBadOutputs(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.flac")
	b := filepath.Join(dir, "b.flac")
	synthAudio(t, a, "flac")
	synthAudio(t, b, "flac")

	notDir := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runMain(t, "normalize", "--album", "--format", "flac", "--dir", notDir, a, b)
	if code != 2 || !strings.Contains(stderr, "not a directory") {
		t.Errorf("exit %d, stderr %q; want exit 2 saying --dir is not a directory", code, stderr)
	}

	// Two tracks with the same base name in different folders.
	nested := filepath.Join(dir, "one", "01.flac")
	other := filepath.Join(dir, "two", "01.flac")
	for _, p := range []string{nested, other} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		synthAudio(t, p, "flac")
	}
	out := filepath.Join(dir, "out")
	_, stderr, code = runMain(t, "normalize", "--album", "--format", "flac", "--dir", out, nested, other)
	if code != 2 || !strings.Contains(stderr, "both map to") {
		t.Errorf("exit %d, stderr %q; want exit 2 naming the collision", code, stderr)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("the refusal came after a write: %v", entries)
	}
}

// A measurement reports what it found, in both modes.
func TestNormalizeAlbumMeasureRendersWarnings(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "a.flac")
	synthAudio(t, good, "flac")
	truncated := filepath.Join(dir, "b.flac")
	whole, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truncated, whole[:len(whole)*6/10], 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runMain(t, "normalize", "--album", "--measure-loudness", good, truncated)
	if code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "input-damage") {
		t.Errorf("stderr = %q, want the damage warning", stderr)
	}
	if !strings.Contains(stdout, "Album:") {
		t.Errorf("stdout = %q, want the album summary", stdout)
	}

	stdout, stderr, code = runMain(t, "normalize", "--album", "--measure-loudness", "--json", good, truncated)
	if code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, stderr)
	}
	doc := oneJSONDoc(t, stdout)
	warns, ok := doc["warnings"].([]any)
	if !ok || len(warns) == 0 {
		t.Fatalf("warnings = %v, want the damage warning", doc["warnings"])
	}
	first, _ := warns[0].(map[string]any)
	if first["code"] != "input-damage" {
		t.Errorf("warnings[0] = %v, want input-damage", warns[0])
	}
	// The warning's count as numbers, on the track it is about and no other.
	tracks, _ := doc["tracks"].([]any)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %v, want two", doc["tracks"])
	}
	if whole, _ := tracks[0].(map[string]any); whole["inputLoss"] != nil {
		t.Errorf("tracks[0] = %v, want no inputLoss on the whole track", whole)
	}
	short, _ := tracks[1].(map[string]any)
	loss, _ := short["inputLoss"].(map[string]any)
	if n, _ := loss["truncatedSamples"].(float64); n <= 0 || loss["lostSamples"] != float64(0) || loss["sampleRate"] != float64(44100) {
		t.Errorf("tracks[1].inputLoss = %v, want what the track ends short by at 44100 Hz", short["inputLoss"])
	}
}
