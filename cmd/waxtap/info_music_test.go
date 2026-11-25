package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
)

func artTrackMusic() *waxtap.Music {
	return &waxtap.Music{
		Title: "Night Drive", Artists: []string{"Small Artist", "Guest Artist"}, Album: "Night Drive EP",
		ReleaseDate: time.Date(2019, 3, 13, 0, 0, 0, 0, time.UTC),
		Copyright:   "℗ 2019 Fixture Records",
		Source:      waxtap.MusicFromDescription,
	}
}

func cardMusic() *waxtap.Music {
	return &waxtap.Music{
		Title: "Night Drive", Artists: []string{"Small Artist"}, Album: "Night Drive EP",
		Source: waxtap.MusicFromWatchPage,
	}
}

func infoHuman(t *testing.T, v *waxtap.Video) string {
	t.Helper()
	var out bytes.Buffer
	renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}},
		&waxtap.InfoResult{Video: v, Client: "VISIONOS"}, 0, errNoBestAudio, nil, false)
	return out.String()
}

func TestInfoHumanShowsWhatTheVideoStatesAboutItsTrack(t *testing.T) {
	got := infoHuman(t, &waxtap.Video{ID: "dummyVideo0", Title: "Night Drive", Author: "Small Artist - Topic", Music: artTrackMusic()})
	for _, want := range []string{
		"Author:    Small Artist - Topic\n",
		"Artist:    Small Artist, Guest Artist\n",
		"Album:     Night Drive EP\n",
		"Released:  2019-03-13\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The track is the video's own title, already on the first line.
	if strings.Contains(got, "Track:") {
		t.Errorf("an Art Track's title needs no second line:\n%s", got)
	}
}

// Spacing is not a difference worth a line: the track is still the title
// already printed.
func TestInfoHumanIgnoresSpacingBetweenTitleAndTrack(t *testing.T) {
	got := infoHuman(t, &waxtap.Video{ID: "dummyVideo0", Title: "Night  Drive ", Author: "Small Artist - Topic", Music: artTrackMusic()})
	if strings.Contains(got, "Track:") {
		t.Errorf("a title that differs only in spacing needs no second line:\n%s", got)
	}
}

// YouTube cuts a long Art Track title short; the whole one is worth showing.
func TestInfoHumanNamesTheWholeTitleOfACutOne(t *testing.T) {
	m := artTrackMusic()
	m.Title = "A Long Title: IV. Presto - Allegro assai (Live)"
	got := infoHuman(t, &waxtap.Video{ID: "dummyVideo0", Title: "A Long Title: IV. Presto - Allegro...", Music: m})
	if !strings.Contains(got, "Track:     A Long Title: IV. Presto - Allegro assai (Live)\n") {
		t.Errorf("missing the whole title in:\n%s", got)
	}
}

func TestInfoHumanNamesTheSongWhenTheTitleSaysMore(t *testing.T) {
	got := infoHuman(t, &waxtap.Video{
		ID: "dummyVideo0", Title: "Small Artist - Night Drive (Official Video)", Author: "SmallArtistVEVO", Music: cardMusic(),
	})
	for _, want := range []string{"Track:     Night Drive\n", "Artist:    Small Artist\n", "Album:     Night Drive EP\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Released:") {
		t.Errorf("the card states no release date, so none is printed:\n%s", got)
	}
}

func TestInfoHumanPrintsNoTrackLinesWithoutMusic(t *testing.T) {
	got := infoHuman(t, &waxtap.Video{ID: "dummyVideo0", Title: "A Video", Author: "Some Channel"})
	for _, label := range []string{"Track:", "Artist:", "Album:", "Released:"} {
		if strings.Contains(got, label) {
			t.Errorf("a video that states no track must print no %s line:\n%s", label, got)
		}
	}
}

func infoJSON(t *testing.T, v *waxtap.Video) map[string]any {
	t.Helper()
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
	if err := emitInfoJSON(env, &waxtap.InfoResult{Video: v, Client: "VISIONOS"}, 0, errNoBestAudio, nil); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return doc
}

func TestInfoJSONMusic(t *testing.T) {
	t.Run("an Art Track", func(t *testing.T) {
		doc := infoJSON(t, &waxtap.Video{ID: "dummyVideo0", Title: "Night Drive", Music: artTrackMusic()})
		want := map[string]any{
			"title":       "Night Drive",
			"artists":     []any{"Small Artist", "Guest Artist"},
			"album":       "Night Drive EP",
			"releaseDate": "2019-03-13",
			"copyright":   "℗ 2019 Fixture Records",
			"source":      "description",
		}
		if !reflect.DeepEqual(doc["music"], want) {
			t.Errorf("music = %v, want %v", doc["music"], want)
		}
	})

	t.Run("a Music card states less, and the absent keys are omitted", func(t *testing.T) {
		m := cardMusic()
		m.Album = ""
		doc := infoJSON(t, &waxtap.Video{ID: "dummyVideo0", Title: "Small Artist - Night Drive", Music: m})
		want := map[string]any{
			"title":   "Night Drive",
			"artists": []any{"Small Artist"},
			"source":  "watch-page",
		}
		if !reflect.DeepEqual(doc["music"], want) {
			t.Errorf("music = %v, want %v", doc["music"], want)
		}
	})

	t.Run("a video that states no track gains no key", func(t *testing.T) {
		doc := infoJSON(t, &waxtap.Video{ID: "dummyVideo0", Title: "A Video"})
		if _, ok := doc["music"]; ok {
			t.Errorf("music = %v, want the key omitted", doc["music"])
		}
	})
}

func TestWriteInfoSidecarMusic(t *testing.T) {
	read := func(t *testing.T, md *waxtap.VideoMetadata) map[string]any {
		t.Helper()
		out := filepath.Join(t.TempDir(), "track.webm")
		if err := writeInfoSidecar(out, &waxtap.Result{VideoID: "dummyVideo0", Metadata: md}); err != nil {
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
		return doc
	}

	doc := read(t, &waxtap.VideoMetadata{Author: "Small Artist - Topic", Music: artTrackMusic()})
	music, _ := doc["music"].(map[string]any)
	if music["album"] != "Night Drive EP" || music["releaseDate"] != "2019-03-13" || music["source"] != "description" {
		t.Errorf("music = %v, want the Art Track's album, release date and source", doc["music"])
	}

	if doc := read(t, &waxtap.VideoMetadata{Author: "Some Channel"}); doc["music"] != nil {
		t.Errorf("music = %v, want the key omitted for a video that states no track", doc["music"])
	}
}
