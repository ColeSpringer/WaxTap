package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"text/tabwriter"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/youtube"
)

// syncWriter serializes per-item playlist output across parallel download
// goroutines, so lines (or NDJSON records) never interleave.
type syncWriter struct {
	env *appEnv
	mu  sync.Mutex
}

// emitItem reports one playlist item's outcome: a human line or one NDJSON
// record. notes are the diagnostics collected for this item alone, drained from
// a scope the caller made for it, because these records are written from
// concurrent out-of-order handlers.
func (s *syncWriter) emitItem(entry youtube.PlaylistEntry, res *waxtap.Result, skipped string, err error, notes []noteJSON) {
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
			Error         *errorJSON    `json:"error,omitempty"`
			Warnings      []warningJSON `json:"warnings,omitempty"`
			Notes         []noteJSON    `json:"notes,omitempty"`
		}{SchemaVersion: schemaVersion, Type: "item", Index: num, VideoID: entry.VideoID, Title: entry.Title, Notes: notes}
		switch {
		case err != nil:
			// The classifier, not err.Error(): a raw string here was the one
			// failure in a WaxTap document a consumer could not switch on.
			rec.Status, rec.Error = "error", errorObject(err)
		case skipped != "":
			rec.Status = "skipped"
		default:
			rec.Status = "ok"
			if res != nil {
				rec.OutputPath = displayPath(res.OutputPath)
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
		fmt.Fprintf(s.env.out, "[%02d] ok: %s -> %s\n", num, title, displayPath(path))
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
	// outcomes are the reached entries, carried so the run's exit code can come
	// from the item errors themselves rather than from a generic "some failed".
	// Nil is allowed: a summary with no outcomes falls back to exit 1.
	outcomes []waxtap.PlaylistItemOutcome
}

// playlistRepresentativeError returns the item error with the highest CLI exit
// code, the same rule local batch runs use (representativeError in batch.go);
// both delegate to worstClassifiedError so the rule cannot drift.
//
// Enumeration errors are deliberately not eligible. They are not item failures:
// a run where every requested item downloaded but three entries failed to enrich
// would otherwise exit with the enrichment's code instead of succeeding-with-a-
// warning, which is what it is.
func playlistRepresentativeError(outcomes []waxtap.PlaylistItemOutcome) error {
	errs := make([]error, len(outcomes))
	for i, o := range outcomes {
		errs[i] = o.Err
	}
	return worstClassifiedError(errs)
}

// worstClassifiedError returns the error with the highest CLI exit code, or nil
// when every entry is nil. It is the shared core of the batch and playlist
// representative-error picks: a run's exit is its worst item's classified code.
func worstClassifiedError(errs []error) error {
	var rep error
	best := -1
	for _, err := range errs {
		if err == nil {
			continue
		}
		if code := exitCodeFor(err); code > best {
			best, rep = code, err
		}
	}
	return rep
}

// emitSummary writes the aggregate result. Item failures and incomplete
// enumeration fail the command; reaching --max-downloads does not.
//
// An item failure exits with that item's own classified code, not the generic 1
// a "N of M failed" message used to produce: the per-item errors were classified
// all along and simply never read, so a one-item playlist whose single video
// needs a PO token exited 1 while the same video downloaded directly exited 8.
// Local batch runs have always worked this way; this is the playlist path
// catching up to them, down to returning alreadyRendered so main does not print
// a second summary line over the one already written.
func (s *syncWriter) emitSummary(sum playlistSummary) error {
	failed := sum.buildRequestFailed + sum.downloadFailed
	if s.env.jsonMode() {
		// The playlist summary reports buildRequestFailed (the per-entry
		// BuildRequest callback failures), distinct from downloadFailed.
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
			// Notes are the run-level ones: those raised before any item and those
			// raised after an item's record was already written.
			Notes []noteJSON `json:"notes,omitempty"`
		}{
			SchemaVersion: schemaVersion, Type: "summary",
			Total: sum.total, OK: sum.ok, Skipped: sum.skipped, Failed: failed,
			BuildRequestFailed: sum.buildRequestFailed, DownloadFailed: sum.downloadFailed,
			Remaining: sum.remaining, CapReached: sum.capReached, EnumerationErrors: sum.enumErrors,
			Notes: s.env.notesJSON(),
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
			fmt.Fprintf(s.env.out, "warning: %s during playlist enumeration; some entries may be missing\n", countOf(sum.enumErrors, "error"))
		}
	}
	switch {
	case failed > 0:
		if rep := playlistRepresentativeError(sum.outcomes); rep != nil {
			return alreadyRendered(rep)
		}
		// No outcome carried the error (a caller that did not pass them). The
		// count is still real, so the run still fails, at the generic code.
		return fmt.Errorf("%d of %d playlist items failed", failed, sum.total)
	case sum.enumErrors > 0:
		return fmt.Errorf("playlist enumeration incomplete: %s; some entries may be missing", countOf(sum.enumErrors, "error"))
	default:
		return nil
	}
}

// countOf formats n with unit, pluralized by adding an "s" unless n is exactly
// one. Every count the CLI prints goes through it, so "1 files" cannot come
// back one summary line at a time.
func countOf(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
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
		// Enumeration errors reach the human listing as warning lines and used to
		// reach the JSON one not at all, so a --json consumer could not tell a
		// complete listing from one missing entries it was never told about.
		errs := make([]errorJSON, 0, len(pl.Errors))
		for _, perr := range pl.Errors {
			// Playlist.Errors never holds a nil, but errorObject documents nil
			// for nil, and a guard is cheaper than a dereference resting on a
			// slice invariant defined two packages away.
			if o := errorObject(perr); o != nil {
				errs = append(errs, *o)
			}
		}
		return env.emitJSON(struct {
			SchemaVersion int         `json:"schemaVersion"`
			PlaylistID    string      `json:"playlistId"`
			Title         string      `json:"title,omitempty"`
			Count         int         `json:"count"`
			Entries       []entryJSON `json:"entries"`
			Errors        []errorJSON `json:"errors,omitempty"`
		}{schemaVersion, pl.ID, pl.Title, len(pl.Entries), entries, errs})
	}

	if pl.Title != "" {
		env.printf("%s (%s)\n\n", pl.Title, countOf(len(pl.Entries), "item"))
	}
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tID\tDURATION\tTITLE")
	for _, e := range pl.Entries {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", e.Index+1, e.VideoID, durationOrDash(e.Duration), e.Title)
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
	Author          string        `json:"author,omitempty"`
	ChannelID       string        `json:"channelId,omitempty"`
	DurationSeconds float64       `json:"durationSeconds,omitempty"`
	PublishDate     string        `json:"publishDate,omitempty"`
	Description     string        `json:"description,omitempty"`
	Chapters        []chapterJSON `json:"chapters,omitempty"`
	Formats         []formatJSON  `json:"formats,omitempty"`
}

// writeInfoSidecar atomically writes <output>.info.json next to a download.
func writeInfoSidecar(outputPath string, res *waxtap.Result) error {
	doc := infoSidecarJSON{resultJSON: resultToJSON(res)}
	if m := res.Metadata; m != nil {
		doc.Author = m.Author
		doc.ChannelID = m.ChannelID
		doc.DurationSeconds = m.Duration.Seconds()
		doc.Description = m.Description
		doc.Chapters = chaptersToJSON(m.Chapters)
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
