package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"text/tabwriter"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/youtube"
)

// syncWriter serializes per-item playlist output across parallel download
// goroutines, so lines (or NDJSON records) never interleave.
type syncWriter struct {
	env *appEnv
	mu  sync.Mutex
}

// emitItem reports one playlist item's outcome: a human line or one NDJSON record.
func (s *syncWriter) emitItem(entry youtube.PlaylistEntry, res *waxtap.Result, skipped string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	num := entry.Index + 1
	if s.env.jsonMode() {
		rec := struct {
			SchemaVersion int    `json:"schemaVersion"`
			Type          string `json:"type"`
			Index         int    `json:"index"`
			VideoID       string `json:"videoId"`
			Title         string `json:"title,omitempty"`
			Status        string `json:"status"`
			OutputPath    string `json:"outputPath,omitempty"`
			Error         string `json:"error,omitempty"`
		}{SchemaVersion: schemaVersion, Type: "item", Index: num, VideoID: entry.VideoID, Title: entry.Title}
		switch {
		case err != nil:
			rec.Status, rec.Error = "error", err.Error()
		case skipped != "":
			rec.Status = "skipped"
		default:
			rec.Status = "ok"
			if res != nil {
				rec.OutputPath = res.OutputPath
			}
		}
		if b, mErr := json.Marshal(rec); mErr == nil {
			fmt.Fprintf(s.env.out, "%s\n", b)
		}
		return
	}

	title := entry.Title
	if title == "" && res != nil {
		title = res.Title
	}
	switch {
	case err != nil:
		fmt.Fprintf(s.env.out, "[%02d] FAIL: %s: %s\n", num, title, friendlyError(err))
	case skipped != "":
		fmt.Fprintf(s.env.out, "[%02d] skip (%s): %s\n", num, skipped, title)
	default:
		path := ""
		if res != nil {
			path = res.OutputPath
		}
		fmt.Fprintf(s.env.out, "[%02d] ok: %s -> %s\n", num, title, path)
	}
}

// emitSummary writes the aggregate result and returns an error when any item
// failed or playlist enumeration was incomplete.
func (s *syncWriter) emitSummary(total, ok, skipped, failed, enumErrors int) error {
	if s.env.jsonMode() {
		rec := struct {
			SchemaVersion     int    `json:"schemaVersion"`
			Type              string `json:"type"`
			Total             int    `json:"total"`
			OK                int    `json:"ok"`
			Skipped           int    `json:"skipped"`
			Failed            int    `json:"failed"`
			EnumerationErrors int    `json:"enumerationErrors,omitempty"`
		}{schemaVersion, "summary", total, ok, skipped, failed, enumErrors}
		if b, err := json.Marshal(rec); err == nil {
			fmt.Fprintf(s.env.out, "%s\n", b)
		}
	} else {
		fmt.Fprintf(s.env.out, "done: %d ok, %d skipped, %d failed (of %d)\n", ok, skipped, failed, total)
		if enumErrors > 0 {
			fmt.Fprintf(s.env.out, "warning: %d playlist enumeration error(s); some entries may be missing\n", enumErrors)
		}
	}
	switch {
	case failed > 0 && enumErrors > 0:
		return fmt.Errorf("%d of %d items failed and enumeration was incomplete (%d error(s))", failed, total, enumErrors)
	case failed > 0:
		return fmt.Errorf("%d of %d playlist items failed", failed, total)
	case enumErrors > 0:
		return fmt.Errorf("playlist enumeration incomplete: %d error(s); some entries may be missing", enumErrors)
	default:
		return nil
	}
}

// emitPlaylistList prints enumerated entries without downloading (the --list flag).
func emitPlaylistList(env *appEnv, pl *waxtap.Playlist) error {
	if env.jsonMode() {
		type entryJSON struct {
			Index           int     `json:"index"`
			VideoID         string  `json:"videoId"`
			Title           string  `json:"title"`
			Author          string  `json:"author,omitempty"`
			DurationSeconds float64 `json:"durationSeconds,omitempty"`
		}
		entries := make([]entryJSON, len(pl.Entries))
		for i, e := range pl.Entries {
			entries[i] = entryJSON{e.Index + 1, e.VideoID, e.Title, e.Author, e.Duration.Seconds()}
		}
		return env.emitJSON(struct {
			SchemaVersion int         `json:"schemaVersion"`
			PlaylistID    string      `json:"playlistId"`
			Title         string      `json:"title,omitempty"`
			Count         int         `json:"count"`
			Entries       []entryJSON `json:"entries"`
		}{schemaVersion, pl.ID, pl.Title, len(pl.Entries), entries})
	}

	if pl.Title != "" {
		env.printf("%s (%d items)\n\n", pl.Title, len(pl.Entries))
	}
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tID\tDURATION\tTITLE")
	for _, e := range pl.Entries {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", e.Index+1, e.VideoID, humanDuration(e.Duration), e.Title)
	}
	tw.Flush()
	for _, perr := range pl.Errors {
		env.info("warning: %v\n", perr)
	}
	return nil
}

// writeInfoSidecar writes a <output>.info.json metadata sidecar next to a download.
func writeInfoSidecar(outputPath string, res *waxtap.Result) error {
	f, err := os.Create(outputPath + ".info.json")
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(resultToJSON(res))
}
