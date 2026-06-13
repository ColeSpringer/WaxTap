package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxtap"
)

// TestWriteInfoSidecar_EnrichedDTO verifies the extended sidecar schema and
// atomic write behavior.
func TestWriteInfoSidecar_EnrichedDTO(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "track.webm")
	res := &waxtap.Result{
		VideoID: "dummyVideo0",
		Title:   "Sidecar Test",
		Client:  "ANDROID_VR",
		Metadata: &waxtap.VideoMetadata{
			Author:      "Test Author",
			Duration:    634500 * time.Millisecond,
			PublishDate: time.Date(2008, 5, 31, 0, 0, 0, 0, time.UTC),
			Description: "a description",
			Formats: []waxtap.Format{
				{Itag: 251, Codec: "opus", Extension: "webm", AverageBitrate: 160000},
			},
		},
	}

	if err := writeInfoSidecar(out, res); err != nil {
		t.Fatalf("writeInfoSidecar: %v", err)
	}

	b, err := os.ReadFile(out + ".info.json")
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	var doc struct {
		SchemaVersion   int     `json:"schemaVersion"`
		SourceKind      string  `json:"sourceKind"` // preserved v3 result field
		Author          string  `json:"author"`
		DurationSeconds float64 `json:"durationSeconds"`
		PublishDate     string  `json:"publishDate"`
		Description     string  `json:"description"`
		Client          string  `json:"client"`
		Formats         []struct {
			Itag int `json:"itag"`
		} `json:"formats"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v", err)
	}
	// Backward compatibility: the v3 result fields are still present.
	if doc.SchemaVersion != schemaVersion || doc.SourceKind == "" {
		t.Errorf("sidecar = %+v, want the embedded v3 result fields preserved (schemaVersion/sourceKind)", doc)
	}
	if doc.Author != "Test Author" || doc.Description != "a description" || doc.Client != "ANDROID_VR" {
		t.Errorf("sidecar = %+v, want author/description/client populated", doc)
	}
	if doc.DurationSeconds != 634.5 {
		t.Errorf("durationSeconds = %v, want 634.5 (float seconds, not ns)", doc.DurationSeconds)
	}
	if doc.PublishDate != "2008-05-31" {
		t.Errorf("publishDate = %q, want 2008-05-31", doc.PublishDate)
	}
	if len(doc.Formats) != 1 || doc.Formats[0].Itag != 251 {
		t.Errorf("formats = %+v, want the single candidate itag 251", doc.Formats)
	}

	// Atomic write: only the final file exists, no leftover temp.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "track.webm.info.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want only track.webm.info.json (no stray temp)", names)
	}
}

// TestWriteInfoSidecar_NoMetadataPreservesResultShape verifies a Result without
// metadata still writes the backward-compatible v3 result document (with the new
// metadata fields omitted), so old --write-info-json consumers are unaffected.
func TestWriteInfoSidecar_NoMetadataPreservesResultShape(t *testing.T) {
	out := filepath.Join(t.TempDir(), "track.webm")
	res := &waxtap.Result{VideoID: "dummyVideo0", Title: "Lean", Client: "WEB"}
	if err := writeInfoSidecar(out, res); err != nil {
		t.Fatalf("writeInfoSidecar: %v", err)
	}
	b, err := os.ReadFile(out + ".info.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// The embedded v3 result fields are present.
	for _, k := range []string{"schemaVersion", "sourceKind", "videoId", "sourceFormat"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("sidecar missing preserved result field %q", k)
		}
	}
	// Opt-in metadata fields are omitted without IncludeMetadata.
	if _, ok := doc["author"]; ok {
		t.Errorf("no-metadata sidecar should omit author, got %v", doc["author"])
	}
}
