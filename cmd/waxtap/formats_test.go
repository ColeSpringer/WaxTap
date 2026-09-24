package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
)

func TestSchemaVersion(t *testing.T) {
	// Bumped to 3 for the error-object pass: playlist and batch item errors became
	// {code, message}, failed items stopped naming an output they never wrote,
	// batch summary counts became unconditional and gained a total, and
	// chapterCount is omitted rather than asserting 0 when chapters were not
	// fetched. See the constant's own comment for the full list.
	if schemaVersion != 3 {
		t.Errorf("schemaVersion = %d, want 3", schemaVersion)
	}
}

func TestFormatJSON_ZeroFieldsStayPresentForYouTube(t *testing.T) {
	// SABR, adaptive, and live formats can report zero for unknown content length,
	// sample rate, channel count, and bitrate. The CLI keeps those fields in JSON;
	// only itag is omitted when there is no YouTube format behind the source.
	b, err := json.Marshal(formatToJSON(waxtap.Format{Itag: 251, Codec: "opus"}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"contentLength", "sampleRate", "channels", "bitrate", "averageBitrate"} {
		if _, ok := m[k]; !ok {
			t.Errorf("formatJSON dropped %q for a real format with a zero value: %v", k, m)
		}
	}
	if _, ok := m["itag"]; !ok {
		t.Errorf("itag should be present when non-zero: %v", m)
	}
}

func TestTriOrDash(t *testing.T) {
	cases := map[waxtap.Tri]string{
		waxtap.Yes:     "yes",
		waxtap.No:      "no",
		waxtap.Unknown: "-",
	}
	for in, want := range cases {
		if got := triOrDash(in); got != want {
			t.Errorf("triOrDash(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestDedupFormats(t *testing.T) {
	// Preserve language and DRC variants while removing exact display duplicates.
	in := []waxtap.Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000},
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000},
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, Language: "es", AudioTrack: &waxtap.AudioTrack{ID: "es.3"}},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, IsDRC: waxtap.Yes},
	}
	got := dedupFormats(in)

	if len(got) != 4 {
		t.Fatalf("dedupFormats kept %d rows, want 4: %+v", len(got), got)
	}
	// The first occurrence determines display order.
	if got[0].Itag != 251 || got[1].Itag != 140 {
		t.Errorf("order changed: got first itags %d, %d, want 251, 140", got[0].Itag, got[1].Itag)
	}
	var haveES, haveDRC bool
	for _, f := range got {
		if f.Itag == 251 && f.Language == "es" {
			haveES = true
		}
		if f.Itag == 140 && f.IsDRC == waxtap.Yes {
			haveDRC = true
		}
	}
	if !haveES {
		t.Error("dropped the Spanish-language 251 variant")
	}
	if !haveDRC {
		t.Error("dropped the DRC 140 variant")
	}
}

// TestFormatToJSON_TrackID pins the two track keys: language is the tag alone
// and audioTrackId the whole id, present only when the format names a track.
func TestFormatToJSON_TrackID(t *testing.T) {
	tracked := waxtap.Format{Itag: 251, Codec: "opus", Language: "en-US", AudioTrack: &waxtap.AudioTrack{ID: "en-US.4"}}
	b, err := json.Marshal(formatToJSON(tracked))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"language":"en-US"`, `"audioTrackId":"en-US.4"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json = %s, want %s", b, want)
		}
	}
	b, err = json.Marshal(formatToJSON(waxtap.Format{Itag: 251, Codec: "opus"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "audioTrackId") || strings.Contains(string(b), "language") {
		t.Errorf("json = %s, want neither track key on a single-track format", b)
	}
}

// TestDedupFormats_KeepsTracksOfOneLanguage keeps two rows that share a
// language but name different tracks: the key is the whole id, not the tag,
// and the table prints that id so the two rows can be told apart.
func TestDedupFormats_KeepsTracksOfOneLanguage(t *testing.T) {
	in := []waxtap.Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, Language: "en", IsOriginal: waxtap.No, AudioTrack: &waxtap.AudioTrack{ID: "en.3", IsOriginal: waxtap.No}},
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, Language: "en", IsOriginal: waxtap.No, AudioTrack: &waxtap.AudioTrack{ID: "en.2", IsOriginal: waxtap.No}},
	}
	got := dedupFormats(in)
	if len(got) != 2 {
		t.Fatalf("dedupFormats kept %d rows, want 2 (different track ids): %+v", len(got), got)
	}
	var out bytes.Buffer
	if err := renderFormatsTable(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, got); err != nil {
		t.Fatal(err)
	}
	table := out.String()
	header, _, _ := strings.Cut(table, "\n")
	if !strings.Contains(header, "TRACK") || strings.Contains(header, "LANG") {
		t.Errorf("header = %q, want a TRACK column in place of LANG", header)
	}
	for _, id := range []string{"en.3", "en.2"} {
		if !strings.Contains(table, id) {
			t.Errorf("table lacks track %s:\n%s", id, table)
		}
	}
}

// TestDedupFormats_KeysOnTheTrackID merges two rows whose only difference is a
// Language set without a track: the track id is the identity, not the tag.
func TestDedupFormats_KeysOnTheTrackID(t *testing.T) {
	in := []waxtap.Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000},
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, Language: "es"},
	}
	if got := dedupFormats(in); len(got) != 1 {
		t.Fatalf("dedupFormats kept %d rows, want 1: without a track id there is one track", len(got))
	}
}

// TestDedupFormats_KeepsTheOriginalFlag keeps two rows that differ only in the
// ORIG column: merging them would hide which one is the original track.
func TestDedupFormats_KeepsTheOriginalFlag(t *testing.T) {
	in := []waxtap.Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, IsOriginal: waxtap.No},
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, IsOriginal: waxtap.Yes},
	}
	if got := dedupFormats(in); len(got) != 2 {
		t.Fatalf("dedupFormats kept %d rows, want 2 (they differ in IsOriginal): %+v", len(got), got)
	}
}

func TestFormatToJSON_AudioQuality(t *testing.T) {
	f := waxtap.Format{Itag: 251, Codec: "opus", Extension: "webm", AverageBitrate: 105000, AudioQuality: waxtap.QualityMedium}
	if got := formatToJSON(f).AudioQuality; got != "medium" {
		t.Errorf("audioQuality = %q, want %q", got, "medium")
	}
	bare := waxtap.Format{Itag: 140, Codec: "mp4a.40.2", Extension: "m4a"}
	if got := formatToJSON(bare).AudioQuality; got != "unknown" {
		t.Errorf("audioQuality (no tier) = %q, want %q", got, "unknown")
	}
}

func TestDefaultNamingPicksWebmFromTier(t *testing.T) {
	formats := []waxtap.Format{
		{Itag: 140, MIMEType: `audio/mp4; codecs="mp4a.40.2"`, Codec: "mp4a.40.2", Extension: "m4a", AverageBitrate: 129000, AudioQuality: waxtap.QualityMedium},
		{Itag: 251, MIMEType: `audio/webm; codecs="opus"`, Codec: "opus", Extension: "webm", AverageBitrate: 105000, AudioQuality: waxtap.QualityMedium},
	}
	idx, err := waxtap.BestForTarget(formats, waxtap.MinimizeLoss(), waxtap.Target{})
	if err != nil {
		t.Fatal(err)
	}
	sel := formats[idx]
	if sel.Itag != 251 {
		t.Fatalf("selected itag %d, want 251 (Opus, MEDIUM tier)", sel.Itag)
	}
	got := resolveOutputName("{id}.{ext}", templateData{ID: "dummyVideo0", Ext: sel.Extension})
	if got != "dummyVideo0.webm" {
		t.Errorf("default-named file = %q, want dummyVideo0.webm", got)
	}
}
