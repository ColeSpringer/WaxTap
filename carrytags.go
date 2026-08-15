package waxtap

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

// carryTags copies the input file's embedded metadata (tags, pictures,
// chapters, synced lyrics) onto a freshly written local output. WaxFlow
// rewrites carry no tags, so without this pass every local transcode, remux,
// or cut silently dropped whatever the source held. Like the embed pass it is
// best-effort: a failure warns and leaves valid audio, never failing the
// process; anything dropped or downgraded is reported, never silent.
//
// When a cut changed the timeline (cut non-nil), chapters are remapped onto
// it: shifted by the audio removed before them, dropped when their content was
// removed. Carrying them unmapped would point at removed audio.
//
// remuxed marks an output whose packets are a byte-identical whole-file copy
// of the input. WaxLabel's transfer excludes tags that describe the source's
// own audio (ReplayGain, encoder stamps, an AcoustID fingerprint), which is
// right for a re-encode or cut; a remux leaves the audio untouched, so those
// values still hold and are restored.
func (c *Client) carryTags(ctx context.Context, srcPath, outPath string, cut *appliedCut, remuxed bool, em *emitter) {
	src, err := waxlabel.ParseFile(ctx, srcPath)
	if err != nil {
		// An unreadable tag form carried nothing before either, so there is no
		// demonstrable loss to warn about.
		c.log.Debug("tag carry: source not readable", "path", srcPath, "err", err)
		return
	}
	if src.Tags().Len() == 0 && len(src.Pictures()) == 0 && len(src.Chapters()) == 0 && len(src.SyncedLyrics()) == 0 {
		return
	}
	out := filepath.Base(outPath)
	warn := func(detail string) { em.warn(WarnTagCarry, detail) }

	dst, err := waxlabel.ParseFile(ctx, outPath)
	if err != nil {
		warn(fmt.Sprintf("could not carry metadata into %s: %v", out, err))
		return
	}
	plan, report, err := src.PrepareTransfer(dst)
	if err != nil {
		warn(fmt.Sprintf("could not carry metadata into %s: %v", out, err))
		return
	}
	postDoc, note, err := executeSaveBack(ctx, plan)
	if err != nil {
		warn(fmt.Sprintf("could not carry metadata into %s: %v", out, err))
		return
	}
	notes := transferLosses(report)
	if note != "" {
		notes = append(notes, note)
	}

	// The two post-transfer fix-ups are mutually exclusive: a cut forces a
	// re-encode, so a remuxed output never has one. Both re-edit the document
	// the transfer returned, the blessed path for writing after an in-place
	// commit (and a no-op plan still returns the unchanged document).
	switch {
	case cut != nil && chaptersLanded(report):
		fixNote, ferr := rewriteChapters(ctx, postDoc, outPath, remapChapters(src.Chapters(), cut))
		if ferr != nil {
			notes = append(notes, fmt.Sprintf("chapters describe the uncut input and could not be remapped: %v", ferr))
		} else if fixNote != "" {
			notes = append(notes, fixNote)
		}
	case remuxed:
		fixNote, ferr := restoreOwnAudio(ctx, postDoc, outPath, src, report)
		if ferr != nil {
			notes = append(notes, fmt.Sprintf("own-audio tags (ReplayGain and similar) could not be restored: %v", ferr))
		} else if fixNote != "" {
			notes = append(notes, fixNote)
		}
	}

	if len(notes) > 0 {
		warn(fmt.Sprintf("metadata carried to %s with losses: %s", out, strings.Join(notes, "; ")))
		return
	}
	c.log.Debug("tag carry: metadata carried", "from", srcPath, "to", outPath)
}

// transferLosses lists a transfer's dropped or downgraded items, one note per
// item, capped so a tag-heavy source cannot balloon the warning. Carried items
// need no note, and Excluded ones are WaxLabel policy (own-audio values whose
// remux case restoreOwnAudio handles), so both stay silent.
func transferLosses(r waxlabel.TransferReport) []string {
	var notes []string
	for _, it := range r.Items {
		if it.Disposition != waxlabel.Dropped && it.Disposition != waxlabel.Lossy {
			continue
		}
		notes = append(notes, transferItemNote(it))
	}
	const maxNotes = 6
	if len(notes) > maxNotes {
		more := len(notes) - (maxNotes - 1)
		notes = append(notes[:maxNotes-1], fmt.Sprintf("and %d more", more))
	}
	return notes
}

func transferItemNote(it waxlabel.TransferItem) string {
	verb := "dropped"
	if it.Disposition == waxlabel.Lossy {
		verb = "downgraded"
	}
	var what string
	switch it.Kind {
	case waxlabel.TransferField:
		what = string(it.Key)
	case waxlabel.TransferPicture:
		what = "1 picture"
		if it.Count != 1 {
			what = fmt.Sprintf("%d pictures", it.Count)
		}
	case waxlabel.TransferChapter:
		what = "chapters"
	default:
		what = "synced lyrics"
	}
	if it.Reason != "" {
		return fmt.Sprintf("%s %s (%s)", what, verb, it.Reason)
	}
	return what + " " + verb
}

// chaptersLanded reports whether the transfer wrote a chapter set (carried or
// downgraded); a dropped set needs no follow-up removal.
func chaptersLanded(r waxlabel.TransferReport) bool {
	for _, it := range r.Items {
		if it.Kind == waxlabel.TransferChapter &&
			(it.Disposition == waxlabel.Carried || it.Disposition == waxlabel.Lossy) {
			return true
		}
	}
	return false
}

// docOrParse returns d, re-parsing path only when the transfer returned no
// document. The contract says that cannot happen on the paths that reach here;
// the parse is the safety net that keeps a fix-up from being skipped.
func docOrParse(ctx context.Context, d *waxlabel.Document, path string) (*waxlabel.Document, error) {
	if d != nil {
		return d, nil
	}
	return waxlabel.ParseFile(ctx, path)
}

// rewriteChapters replaces the chapter set the transfer just wrote. It exists
// for the cut case only, where the carried offsets describe the uncut input.
func rewriteChapters(ctx context.Context, doc *waxlabel.Document, path string, chs []waxlabel.Chapter) (string, error) {
	d, err := docOrParse(ctx, doc, path)
	if err != nil {
		return "", err
	}
	ed := d.Edit()
	if len(chs) == 0 {
		ed.ClearChapters()
	} else {
		ed.SetChapters(chs...)
	}
	plan, err := ed.Prepare()
	if err != nil {
		return "", err
	}
	_, note, err := executeSaveBack(ctx, plan)
	return note, err
}

// restoreOwnAudio writes back the own-audio tags the transfer excluded. It
// exists for the whole-file remux case only, where the output's packets are
// the input's and the excluded values still describe them exactly.
func restoreOwnAudio(ctx context.Context, doc *waxlabel.Document, path string, src *waxlabel.Document, report waxlabel.TransferReport) (string, error) {
	var keys []tag.Key
	for _, it := range report.Items {
		if it.Kind == waxlabel.TransferField && it.Disposition == waxlabel.Excluded {
			keys = append(keys, it.Key)
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	d, err := docOrParse(ctx, doc, path)
	if err != nil {
		return "", err
	}
	ed := d.Edit()
	for _, k := range keys {
		if vals, ok := src.Get(k); ok {
			ed.Set(k, vals...)
		}
	}
	plan, err := ed.Prepare()
	if err != nil {
		return "", err
	}
	_, note, err := executeSaveBack(ctx, plan)
	return note, err
}
