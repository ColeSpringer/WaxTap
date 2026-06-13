package main

import (
	"testing"

	"github.com/colespringer/waxtap"
)

func TestSchemaVersionIsThree(t *testing.T) {
	if schemaVersion != 3 {
		t.Errorf("schemaVersion = %d, want 3", schemaVersion)
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
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, Language: "es"},
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
