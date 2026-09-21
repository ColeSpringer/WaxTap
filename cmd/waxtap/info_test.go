package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
)

// A video whose length the source did not report prints a dash, not 0:00, and
// its JSON document omits durationSeconds like the listing and sidecar
// documents do, rather than asserting a zero-length video.
func TestInfoUnknownDuration(t *testing.T) {
	noBest := errors.New("no best audio")
	unknown := &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0", Title: "T", Author: "A"}}
	known := &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0", Title: "T", Author: "A", Duration: 90 * time.Second}}

	t.Run("human", func(t *testing.T) {
		var out bytes.Buffer
		renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, unknown, 0, noBest, nil, false)
		if got := out.String(); !strings.Contains(got, "Duration:  -\n") {
			t.Errorf("want the unknown duration rendered as a dash, got:\n%s", got)
		}
	})
	t.Run("json", func(t *testing.T) {
		for name, res := range map[string]*waxtap.InfoResult{"unknown": unknown, "known": known} {
			var out bytes.Buffer
			if err := emitInfoJSON(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}, res, 0, noBest, nil); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if has := strings.Contains(got, `"durationSeconds"`); has != (res.Video.Duration > 0) {
				t.Errorf("%s: durationSeconds present = %v, want it only for a reported length, got:\n%s", name, has, got)
			}
		}
	})
}

// TestInfoChaptersDetail verifies info --full's chapters are detailed in the
// human list and the JSON chapters array, with an open-ended last chapter that
// omits its end.
func TestInfoChaptersDetail(t *testing.T) {
	noBest := errors.New("no best audio")
	withChapters := &waxtap.InfoResult{
		Video: &waxtap.Video{
			ID: "dummyVideo0", Title: "T", Author: "A",
			Chapters: []waxtap.Chapter{
				{Title: "Intro", Start: 0, End: 30 * time.Second},
				{Title: "Outro", Start: 90 * time.Second}, // open-ended (End == 0)
			},
		},
		Client: "ANDROID_VR",
		// Chapters only ever come from the watch-page pass, so a fixture carrying
		// them is a --full result.
		FullMetadata: true,
	}

	t.Run("human list", func(t *testing.T) {
		var out bytes.Buffer
		renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, withChapters, 0, noBest, nil, false)
		got := out.String()
		if !strings.Contains(got, "Chapters:  2") {
			t.Errorf("want the chapter count, got:\n%s", got)
		}
		if !strings.Contains(got, "0:00-0:30  Intro") {
			t.Errorf("want the ranged Intro line, got:\n%s", got)
		}
		// The open-ended last chapter shows only its start.
		if !strings.Contains(got, "1:30  Outro") || strings.Contains(got, "1:30-") {
			t.Errorf("want an open-ended Outro line (start only), got:\n%s", got)
		}
	})

	t.Run("json array", func(t *testing.T) {
		var out bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}, withChapters, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if !strings.Contains(got, `"chapterCount": 2`) {
			t.Errorf("want chapterCount, got:\n%s", got)
		}
		if !strings.Contains(got, `"title": "Intro"`) || !strings.Contains(got, `"endSeconds": 30`) {
			t.Errorf("want the Intro chapter with endSeconds, got:\n%s", got)
		}
		if !strings.Contains(got, `"title": "Outro"`) {
			t.Errorf("want the Outro chapter, got:\n%s", got)
		}
		// Only Intro carries an end; the open-ended Outro omits endSeconds.
		if n := strings.Count(got, "endSeconds"); n != 1 {
			t.Errorf("endSeconds count = %d, want 1 (open-ended chapter omits it):\n%s", n, got)
		}
	})

	t.Run("absent when no chapters", func(t *testing.T) {
		plain := &waxtap.InfoResult{
			Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "ANDROID_VR", FullMetadata: true,
		}
		var out bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}, plain, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), `"chapters"`) {
			t.Errorf("JSON should omit the chapters array when empty, got:\n%s", out.String())
		}
		// The pass ran and found none, so 0 is a real answer and is reported.
		if !strings.Contains(out.String(), `"chapterCount": 0`) {
			t.Errorf("a completed full pass must report 0 chapters, got:\n%s", out.String())
		}
	})

	// Without the full pass nothing ever looked for chapters, so reporting 0
	// asserts an answer that was never sought: a consumer building a chapter
	// index read it as "this video has none".
	t.Run("omitted without full metadata", func(t *testing.T) {
		basic := &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "ANDROID_VR"}
		var out bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}, basic, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if strings.Contains(got, `"chapterCount"`) {
			t.Errorf("chapterCount asserted without a full pass, got:\n%s", got)
		}
		if !strings.Contains(got, `"fullMetadata": false`) {
			t.Errorf("want fullMetadata false so a consumer knows why, got:\n%s", got)
		}
	})
}

// TestInfoSubstitutionBreadcrumb verifies that a forced-client fallback to WEB
// is shown in human and JSON output.
func TestInfoSubstitutionBreadcrumb(t *testing.T) {
	noBest := errors.New("no best audio")
	substituted := &waxtap.InfoResult{
		Video:           &waxtap.Video{ID: "dummyVideo0", Title: "T", Author: "A"},
		Client:          "WEB",
		SubstitutedFrom: "WEB_EMBEDDED",
	}

	t.Run("human note", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		renderInfoHuman(env, substituted, 0, noBest, nil, false)
		got := out.String()
		if !strings.Contains(got, "Client:    WEB") {
			t.Errorf("want the Client line, got:\n%s", got)
		}
		if !strings.Contains(got, "requested WEB_EMBEDDED; fell back to WEB") {
			t.Errorf("want the substitution note, got:\n%s", got)
		}
	})

	t.Run("json field", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
		if err := emitInfoJSON(env, substituted, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"substitutedFrom": "WEB_EMBEDDED"`) {
			t.Errorf("want substitutedFrom in JSON, got:\n%s", out.String())
		}
	})

	t.Run("absent when no substitution", func(t *testing.T) {
		plain := &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "ANDROID_VR"}

		var human bytes.Buffer
		renderInfoHuman(&appEnv{out: &human, errOut: io.Discard, cfg: &appConfig{}}, plain, 0, noBest, nil, false)
		if strings.Contains(human.String(), "fell back") {
			t.Errorf("no substitution should print no breadcrumb, got:\n%s", human.String())
		}

		var js bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &js, errOut: io.Discard, cfg: &appConfig{json: true}}, plain, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(js.String(), "substitutedFrom") {
			t.Errorf("JSON should omit substitutedFrom when empty, got:\n%s", js.String())
		}
	})
}

// A formats listing that came from the watch page says so, on every client:
// the watch page needs no PO token, which is the fact worth reporting, and a
// client substitution is a detail of it rather than a separate note.
func TestFormatsWatchPageNote(t *testing.T) {
	cases := []struct {
		name  string
		info  *waxtap.InfoResult
		want  string // "" means no note
		avoid string
	}{
		{
			name: "forced web fell back",
			info: &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "WEB", ViaWatchPage: true},
			want: "listing WEB formats from the watch-page fallback (no PO token)",
		},
		{
			name:  "a substitution names what was asked for",
			info:  &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "WEB", ViaWatchPage: true, SubstitutedFrom: "WEB_EMBEDDED_PLAYER"},
			want:  "requested WEB_EMBEDDED_PLAYER; listing WEB formats from the watch-page fallback (no PO token)",
			avoid: "",
		},
		{
			name: "a direct read says nothing",
			info: &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "WEB"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &appEnv{out: io.Discard, errOut: io.Discard, cfg: &appConfig{}, notes: &noteCollector{}}
			noteFormatsSource(env, tc.info)
			notes := env.notesJSON()
			if tc.want == "" {
				if len(notes) != 0 {
					t.Fatalf("notes = %v, want none", notes)
				}
				return
			}
			if len(notes) != 1 || notes[0].Code != string(noteWatchPageFormats) {
				t.Fatalf("notes = %v, want one %s", notes, noteWatchPageFormats)
			}
			if notes[0].Detail != tc.want {
				t.Errorf("detail = %q, want %q", notes[0].Detail, tc.want)
			}
		})
	}
}

// TestInfoLiveStatusJSON covers the additive liveStatus/availability keys. They
// surface a was-live VOD and an unlisted video, but stay absent for a normal video
// (with or without --full), keeping the schemaVersion-1 output byte-identical.
func TestInfoLiveStatusJSON(t *testing.T) {
	t.Run("helpers emit only new signals", func(t *testing.T) {
		if got := infoLiveStatus(waxtap.LiveNone); got != "" {
			t.Errorf("infoLiveStatus(LiveNone) = %q, want empty", got)
		}
		if got := infoLiveStatus(waxtap.LiveWasLive); got != "was_live" {
			t.Errorf("infoLiveStatus(LiveWasLive) = %q, want was_live", got)
		}
		if got := infoAvailability(waxtap.AvailabilityUnknown); got != "" {
			t.Errorf("infoAvailability(Unknown) = %q, want empty", got)
		}
		// --full resolves a normal video to Public; it must not gain a key.
		if got := infoAvailability(waxtap.AvailabilityPublic); got != "" {
			t.Errorf("infoAvailability(Public) = %q, want empty (byte-identity)", got)
		}
		if got := infoAvailability(waxtap.AvailabilityUnlisted); got != "unlisted" {
			t.Errorf("infoAvailability(Unlisted) = %q, want unlisted", got)
		}
	})

	emit := func(v *waxtap.Video) string {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
		if err := emitInfoJSON(env, &waxtap.InfoResult{Video: v, Client: "ANDROID_VR"}, 0, errors.New("no best"), nil); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	t.Run("normal video omits both keys", func(t *testing.T) {
		got := emit(&waxtap.Video{ID: "dummyVideo0", LiveStatus: waxtap.LiveNone, Availability: waxtap.AvailabilityPublic})
		if strings.Contains(got, "liveStatus") || strings.Contains(got, "availability") {
			t.Errorf("a normal video must not gain liveStatus/availability keys:\n%s", got)
		}
	})

	t.Run("was-live and unlisted gain keys", func(t *testing.T) {
		got := emit(&waxtap.Video{ID: "dummyVideo0", LiveStatus: waxtap.LiveWasLive, Availability: waxtap.AvailabilityUnlisted})
		if !strings.Contains(got, `"liveStatus": "was_live"`) {
			t.Errorf("want liveStatus was_live:\n%s", got)
		}
		if !strings.Contains(got, `"availability": "unlisted"`) {
			t.Errorf("want availability unlisted:\n%s", got)
		}
	})
}

// TestNoteInfoChannelLayout covers F5's second case: `info --channels mono`
// selects the best available stream and used to report the mismatch nowhere.
func TestNoteInfoChannelLayout(t *testing.T) {
	formats := []waxtap.Format{{Itag: 251, Channels: 2}, {Itag: 258, Channels: 6}}
	selErr := errors.New("no best audio")
	cases := []struct {
		name     string
		layout   waxtap.ChannelLayout
		explicit bool
		itag     int
		formats  []waxtap.Format
		bestIdx  int
		bestErr  error
		want     string // substring expected in the note; "" means no note
	}{
		{"mono request on a stereo best", waxtap.LayoutMono, true, 0, formats, 0, nil, "requested mono; best audio is stereo (2ch)"},
		{"stereo request on a surround best", waxtap.LayoutStereo, true, 0, formats, 1, nil, "requested stereo; best audio is 6ch"},
		{"satisfied request stays quiet", waxtap.LayoutStereo, true, 0, formats, 0, nil, ""},
		{"default (not explicit) stays quiet", waxtap.LayoutStereo, false, 0, formats, 1, nil, ""},
		{"any never notes", waxtap.LayoutAny, true, 0, formats, 1, nil, ""},
		{"selection failure stays quiet", waxtap.LayoutMono, true, 0, formats, -1, selErr, ""},
		{"index out of range stays quiet", waxtap.LayoutMono, true, 0, formats, 9, nil, ""},
		{"unknown channel count stays quiet", waxtap.LayoutMono, true, 0, []waxtap.Format{{Itag: 140}}, 0, nil, ""},
		// An itag picks the row on its own, so the mismatch note has to say that
		// --channels never ran rather than implying it lost.
		{"itag names the reason", waxtap.LayoutStereo, true, 258, formats, 1, nil, "--itag names an exact encoding, so --channels did not affect selection; requested stereo, best audio is 6ch"},
		{"itag satisfying the layout stays quiet", waxtap.LayoutStereo, true, 251, formats, 0, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			noteInfoChannelLayout(noteEnv(&buf), tc.layout, tc.explicit, tc.itag, tc.formats, tc.bestIdx, tc.bestErr)
			got := buf.String()
			switch {
			case tc.want == "" && got != "":
				t.Errorf("note = %q, want none", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("note = %q, want substring %q", got, tc.want)
			}
		})
	}
}

// A watch-page delivery is not a player delivery: the Client line names WEB (or
// whatever client scraped it), and nothing else says the formats came from a
// scrape. The suffix is the only thing that separates the two.
func TestInfoHumanClientViaWatchPage(t *testing.T) {
	via := &waxtap.InfoResult{
		Video:        &waxtap.Video{ID: "dummyVideo0", Title: "T", Author: "A"},
		Client:       "WEB",
		ViaWatchPage: true,
	}
	var out bytes.Buffer
	renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, via, 0, errNoBestAudio, nil, false)
	if !strings.Contains(out.String(), "Client:    WEB (via watch page)") {
		t.Errorf("want the watch-page suffix on the Client line, got:\n%s", out.String())
	}

	direct := &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "WEB"}
	out.Reset()
	renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, direct, 0, errNoBestAudio, nil, false)
	if strings.Contains(out.String(), "watch page") {
		t.Errorf("a player delivery must carry no suffix, got:\n%s", out.String())
	}
}

func TestInfoJSONViaWatchPageAdditive(t *testing.T) {
	decode := func(info *waxtap.InfoResult) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}, info, 0, errNoBestAudio, nil); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		return m
	}

	via := decode(&waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "WEB", ViaWatchPage: true})
	if via["viaWatchPage"] != true {
		t.Errorf("viaWatchPage = %v, want true", via["viaWatchPage"])
	}
	direct := decode(&waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "WEB"})
	if _, ok := direct["viaWatchPage"]; ok {
		t.Error("viaWatchPage must be omitted for a player delivery, keeping the key additive")
	}
}

// errNoBestAudio stands in for the selector's "nothing eligible" error in
// rendering tests, which never exercise format selection.
var errNoBestAudio = errors.New("no best audio")

// TestInfoSelectionFlags covers F15's CLI half: info can be asked what a
// specific request would pick, and it validates that request through the helpers
// download uses, so the two commands cannot drift into different verdicts on the
// same flags.
func TestInfoSelectionFlags(t *testing.T) {
	t.Run("flags registered", func(t *testing.T) {
		f := newInfoCmd().Flags()
		for _, name := range []string{"itag", "codec", "source-policy"} {
			if f.Lookup(name) == nil {
				t.Errorf("info should expose --%s; without it a preview needs an actual download", name)
			}
		}
	})

	cases := []struct {
		name string
		args []string
	}{
		{"itag and codec are mutually exclusive", []string{"dummyVideo0", "--itag", "251", "--codec", "opus"}},
		{"a zero itag is rejected", []string{"dummyVideo0", "--itag", "0"}},
		{"a negative itag is rejected", []string{"dummyVideo0", "--itag=-5"}},
		{"an unknown source policy is rejected", []string{"dummyVideo0", "--source-policy", "bogus"}},
		{"prefer: needs a known codec", []string{"dummyVideo0", "--source-policy", "prefer:bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every one of these is rejected before the extraction call, so the test
			// never touches the network.
			cmd := newInfoCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Errorf("info %v: err = %v (%T), want *usageError", tc.args, err, err)
			}
		})
	}
}

// TestInfoSelectorDrivesThePreview is the substance of F15: the selector and
// policy the flags build decide the row info reports, so `info --itag 338`
// answers "what would that give me?" instead of describing the default pick.
func TestInfoSelectorDrivesThePreview(t *testing.T) {
	// The 6ch track outranks stereo on bitrate but loses the layout comparison, so
	// a default run never shows it; itag 140 is the aac row a codec preference
	// reaches. Tier fields are left unset, so bitrate decides the rest.
	formats := []waxtap.Format{
		{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 2, AverageBitrate: 160000, IsOriginal: waxtap.Yes},
		{Itag: 338, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 6, AverageBitrate: 256000, IsOriginal: waxtap.Yes},
		{Itag: 140, Codec: "mp4a.40.2", Extension: "m4a", MIMEType: "audio/mp4", Channels: 2, AverageBitrate: 128000, IsOriginal: waxtap.Yes},
	}
	// The same three steps info's RunE takes, in the same order.
	pick := func(t *testing.T, itag int, codec, sourcePolicy string) int {
		t.Helper()
		sel, err := audioSelector(itag, codec, waxtap.LayoutStereo)
		if err != nil {
			t.Fatalf("audioSelector(%d, %q): %v", itag, codec, err)
		}
		policy, err := parseSourcePolicy(sourcePolicy)
		if err != nil {
			t.Fatalf("parseSourcePolicy(%q): %v", sourcePolicy, err)
		}
		idx, err := sel.Select(formats, policy, waxtap.Target{})
		if err != nil {
			t.Fatalf("select (itag=%d codec=%q policy=%q): %v", itag, codec, sourcePolicy, err)
		}
		return formats[idx].Itag
	}

	cases := []struct {
		name         string
		itag         int
		codec        string
		sourcePolicy string
		want         int
	}{
		{"default previews the stereo best", 0, "", "minimize-loss", 251},
		{"itag previews the exact row", 338, "", "minimize-loss", 338},
		{"codec filters to its family", 0, "aac", "minimize-loss", 140},
		{"prefer: biases without filtering", 0, "", "prefer:aac", 140},
		{"prefer: on the winner changes nothing", 0, "", "prefer:opus", 251},
		// info names no transcode target, so minimize-loss and best-native both fall
		// through to plain best-audio ranking. Only prefer:<codec> moves the pick.
		{"best-native matches minimize-loss with no target", 0, "", "best-native", 251},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pick(t, tc.itag, tc.codec, tc.sourcePolicy); got != tc.want {
				t.Errorf("selection = itag %d, want itag %d", got, tc.want)
			}
		})
	}

	// The rows above only prove the selection; this pins that info hands that same
	// selection to the library rather than resolving and probing the default row.
	// ReadOption closes over an unexported type, so package main cannot apply one
	// to inspect it, and the probe itself is behind a network call.
	src, err := os.ReadFile("info.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"waxtap.WithSelector(sel)",                                  // --probe probes the requested row
		"waxtap.WithSourcePolicy(policy)",                           // and ranks it under the requested policy
		"env.client.Resolve(cmd.Context(), args[0], sel, ropts...)", // --show-url signs that row's URL
		"sel.Select(video.Formats, policy,",                         // and the displayed row agrees
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("info.go no longer contains %q; the preview would report a row the flags did not ask for", want)
		}
	}
}
