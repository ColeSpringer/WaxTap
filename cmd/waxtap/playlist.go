package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"text/tabwriter"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/internal/tempfile"
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
			SchemaVersion int           `json:"schemaVersion"`
			Type          string        `json:"type"`
			Index         int           `json:"index"`
			VideoID       string        `json:"videoId"`
			Title         string        `json:"title,omitempty"`
			Status        string        `json:"status"`
			OutputPath    string        `json:"outputPath,omitempty"`
			Client        string        `json:"client,omitempty"`
			Error         string        `json:"error,omitempty"`
			Warnings      []warningJSON `json:"warnings,omitempty"`
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
				rec.Client = res.Client
				for _, w := range res.Warnings {
					rec.Warnings = append(rec.Warnings, warningJSON{Code: w.Code.String(), Detail: w.Detail})
				}
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

// playlistSummary contains the counts printed at the end of a playlist run.
type playlistSummary struct {
	total              int
	ok                 int
	skipped            int
	buildRequestFailed int
	downloadFailed     int
	remaining          int // never attempted (cap reached or canceled mid-run)
	enumErrors         int
	capReached         bool
}

// emitSummary writes the aggregate result. Item failures and incomplete
// enumeration fail the command; reaching --max-downloads does not.
func (s *syncWriter) emitSummary(sum playlistSummary) error {
	failed := sum.buildRequestFailed + sum.downloadFailed
	if s.env.jsonMode() {
		// schemaVersion 4 renamed resolveFailed to buildRequestFailed.
		rec := struct {
			SchemaVersion      int    `json:"schemaVersion"`
			Type               string `json:"type"`
			Total              int    `json:"total"`
			OK                 int    `json:"ok"`
			Skipped            int    `json:"skipped"`
			Failed             int    `json:"failed"`
			BuildRequestFailed int    `json:"buildRequestFailed,omitempty"`
			DownloadFailed     int    `json:"downloadFailed,omitempty"`
			Remaining          int    `json:"remaining,omitempty"`
			CapReached         bool   `json:"capReached,omitempty"`
			EnumerationErrors  int    `json:"enumerationErrors,omitempty"`
		}{
			SchemaVersion: schemaVersion, Type: "summary",
			Total: sum.total, OK: sum.ok, Skipped: sum.skipped, Failed: failed,
			BuildRequestFailed: sum.buildRequestFailed, DownloadFailed: sum.downloadFailed,
			Remaining: sum.remaining, CapReached: sum.capReached, EnumerationErrors: sum.enumErrors,
		}
		if b, err := json.Marshal(rec); err == nil {
			fmt.Fprintf(s.env.out, "%s\n", b)
		}
	} else {
		line := fmt.Sprintf("done: %d ok, %d skipped, %d failed (of %d)", sum.ok, sum.skipped, failed, sum.total)
		if sum.remaining > 0 {
			line += fmt.Sprintf("; %d remaining", sum.remaining)
			if sum.capReached {
				line += " (max-downloads reached)"
			}
		}
		fmt.Fprintf(s.env.out, "%s\n", line)
		if sum.enumErrors > 0 {
			fmt.Fprintf(s.env.out, "warning: %d playlist enumeration error(s); some entries may be missing\n", sum.enumErrors)
		}
	}
	switch {
	case failed > 0 && sum.enumErrors > 0:
		return fmt.Errorf("%d of %d items failed and enumeration was incomplete (%d error(s))", failed, sum.total, sum.enumErrors)
	case failed > 0:
		return fmt.Errorf("%d of %d playlist items failed", failed, sum.total)
	case sum.enumErrors > 0:
		return fmt.Errorf("playlist enumeration incomplete: %d error(s); some entries may be missing", sum.enumErrors)
	default:
		return nil
	}
}

// itemCount formats n with the correct singular or plural unit.
func itemCount(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
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
		env.printf("%s (%s)\n\n", pl.Title, itemCount(len(pl.Entries)))
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

// infoSidecarJSON extends a result document with metadata requested by
// --write-info-json. Embedding resultJSON preserves the standard result fields.
type infoSidecarJSON struct {
	resultJSON
	Author          string       `json:"author,omitempty"`
	DurationSeconds float64      `json:"durationSeconds,omitempty"`
	PublishDate     string       `json:"publishDate,omitempty"`
	Description     string       `json:"description,omitempty"`
	Formats         []formatJSON `json:"formats,omitempty"`
}

// writeInfoSidecar atomically writes <output>.info.json next to a download.
func writeInfoSidecar(outputPath string, res *waxtap.Result) error {
	doc := infoSidecarJSON{resultJSON: resultToJSON(res)}
	if m := res.Metadata; m != nil {
		doc.Author = m.Author
		doc.DurationSeconds = m.Duration.Seconds()
		doc.Description = m.Description
		if !m.PublishDate.IsZero() {
			doc.PublishDate = m.PublishDate.Format("2006-01-02")
		}
		for _, f := range m.Formats {
			doc.Formats = append(doc.Formats, formatToJSON(f))
		}
	}

	tf, err := tempfile.New(outputPath + ".info.json")
	if err != nil {
		return err
	}
	defer tf.Discard() // no-op after Commit
	enc := json.NewEncoder(tf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return tf.Commit()
}
