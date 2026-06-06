package main

import (
	"testing"

	"github.com/colespringer/waxtap"
)

func TestSchemaVersionIsTwo(t *testing.T) {
	if schemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2", schemaVersion)
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
