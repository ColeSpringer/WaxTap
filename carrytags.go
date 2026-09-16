package waxtap

import (
	"context"
	"fmt"
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
//
// dest is the path warnings name. It differs from outPath when the output is
// staged for an exclusive publish, where outPath is a temp name the user never
// asked for and will never see; "" falls back to outPath.
//
// The carry runs before the publish, so under a renumbering output dest is the
// requested base name and a warning can name it where the file landed one
// number over. Cosmetic, and the alternative is deferring the whole carry until
// after the path is known, which would mean publishing an untagged file first.
//
// It returns the itemized report Result.TagCarry exposes, nil when no carry
// ran: the same facts the warning tells, one item per field and per set.
func (c *Client) carryTags(ctx context.Context, srcPath, outPath, dest string, cut *appliedCut, remuxed bool, em *emitter) *TagCarry {
	src, err := waxlabel.ParseFile(ctx, srcPath)
	if err != nil {
		// An unreadable source carried nothing before either, so there is no
		// demonstrable loss to warn about. WaxLabel identifies every format the
		// engine decodes (the APEv2 family, Musepack included, and WMA), so this
		// is a damaged file, not a format gap.
		c.log.Debug("tag carry: source not readable", "path", srcPath, "err", err)
		return nil
	}
	if src.Tags().Len() == 0 && len(src.Pictures()) == 0 && len(src.Chapters()) == 0 && len(src.SyncedLyrics()) == 0 {
		return nil
	}
	out := warnName(dest, outPath)
	warn := func(detail string) { em.warn(WarnTagCarry, detail) }
	tc := &TagCarry{}
	failed := func(err error) *TagCarry {
		tc.Error = fmt.Sprintf("could not carry metadata into %s: %v", out, err)
		warn(tc.Error)
		return tc
	}

	dst, err := waxlabel.ParseFile(ctx, outPath)
	if err != nil {
		return failed(err)
	}
	// A cut moved the timeline, so the source's chapters and synced lyrics
	// are remapped onto it ahead of the transfer and handed over as the lists
	// to write, in the same pass as the tags. Carried as they are they would
	// point at removed audio; rewriting them after the transfer, which is what
	// this did before WaxLabel's transfer took a replacement, cost a second
	// metadata write. A set the remap empties goes over as an explicit
	// nothing, which the transfer grades as no item at all.
	tr := src.Transfer()
	var chapters, lyrics *cutRemap
	if cut != nil {
		if chs := src.Chapters(); len(chs) > 0 {
			kept := remapChapters(chs, cut)
			tr.SetChapters(kept...)
			chapters = &cutRemap{kept: len(kept), removed: len(chs) - len(kept), input: len(chs)}
		}
		if sls := src.SyncedLyrics(); len(sls) > 0 {
			kept, dropped := remapSyncedLyrics(sls, cut)
			tr.SetSyncedLyrics(kept...)
			lyrics = &cutRemap{kept: len(kept), removed: dropped, input: lyricSets(sls)}
		}
	}
	plan, report, err := tr.Prepare(dst)
	if err != nil {
		return failed(err)
	}
	postDoc, note, err := executeSaveBack(ctx, plan)
	if err != nil {
		return failed(err)
	}
	tc.Items = carryItems(report)
	notes := transferLosses(report)
	notes = append(notes, unprojectedSourceNotes(src)...)
	if note != "" {
		notes = append(notes, note)
	}

	switch {
	case cut != nil:
		if chapters != nil {
			tc.remapped(CarryChapters, *chapters, chaptersLanded(report))
		}
		if lyrics != nil {
			landed := lyricsLanded(report)
			tc.remapped(CarrySyncedLyrics, *lyrics, landed)
			// A line the cut took is worth a note where lines landed, and
			// where the cut emptied the set; a set the destination dropped
			// whole says so in its own item.
			if lyrics.removed > 0 && (landed || lyrics.kept == 0) {
				notes = append(notes, lyricsDropNote(lyrics.removed, lyrics.input-lyrics.kept, lyrics.input))
			}
		}
	case remuxed:
		// The one post-transfer fix-up left re-edits the document the
		// transfer returned, the blessed path for writing after an in-place
		// commit (and a no-op plan still returns the unchanged document).
		restored, fixNote, ferr := restoreOwnAudio(ctx, postDoc, outPath, src, report)
		if ferr != nil {
			notes = append(notes, fmt.Sprintf("own-audio tags (ReplayGain and similar) could not be restored: %v", ferr))
		} else {
			tc.restored(restored)
			if fixNote != "" {
				notes = append(notes, fixNote)
			}
		}
	}

	if len(notes) > 0 {
		lead := "metadata carried to %s with losses: %s"
		if !landedRemains(report) {
			// Nothing landed, so a "carried with losses" claim would be
			// false: a source whose only metadata is a chapter set, carried
			// into a format that stores none, or one the cut emptied.
			lead = "no metadata carried to %s: %s"
		}
		warn(fmt.Sprintf(lead, out, strings.Join(capNotes(notes), "; ")))
		return tc
	}
	c.log.Debug("tag carry: metadata carried", "from", srcPath, "to", outPath)
	return tc
}

// cutRemap is what a cut's remap of one set produced: the pieces handed to
// the transfer (chapters, or lyric sets still holding a line), the pieces the
// cut took (chapters, or lyric lines), and the pieces the source had.
type cutRemap struct {
	kept, removed, input int
}

// lyricSets counts the sets that hold a line, the ones a transfer writes.
func lyricSets(sls []waxlabel.SyncedLyrics) int {
	n := 0
	for _, sl := range sls {
		if len(sl.Lines) > 0 {
			n++
		}
	}
	return n
}

// landedRemains reports whether the transfer wrote anything (carried or
// downgraded; excluded and dropped items move nothing). A chapter or lyric set
// the cut emptied never entered the transfer, so it cannot count.
func landedRemains(r waxlabel.TransferReport) bool {
	for _, it := range r.Items {
		if it.Disposition == waxlabel.Carried || it.Disposition == waxlabel.Lossy {
			return true
		}
	}
	return false
}

// unprojectedSourceNotes relays the source's read warnings about tag content
// no reader can project (a native key the canonical vocabulary cannot
// represent, a malformed entry): such an item never enters the canonical set,
// so the transfer report cannot account for it and it stays behind on the
// source.
func unprojectedSourceNotes(src *waxlabel.Document) []string {
	var notes []string
	for _, w := range src.Warnings() {
		if w.Code == waxlabel.WarnInvalidTagKey || w.Code == waxlabel.WarnMalformedTagEntry {
			notes = append(notes, w.Message)
		}
	}
	return notes
}

// transferLosses lists a transfer's dropped or downgraded items, one note per
// item. Carried items need no note, and Excluded ones are WaxLabel policy
// (own-audio values whose remux case restoreOwnAudio handles), so both stay
// silent. The caller caps the merged note list (capNotes).
func transferLosses(r waxlabel.TransferReport) []string {
	var notes []string
	for _, it := range r.Items {
		if it.Disposition != waxlabel.Dropped && it.Disposition != waxlabel.Lossy {
			continue
		}
		notes = append(notes, transferItemNote(it))
	}
	return notes
}

// capNotes bounds a note list so a tag-heavy source cannot balloon one warning.
func capNotes(notes []string) []string {
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
// downgraded), so a cut's remap has an item on the output to describe; a
// dropped set, or one the cut emptied ahead of the transfer, has none.
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

// lyricsLanded reports whether the transfer wrote a synced-lyrics set (carried
// or downgraded), for the reason chaptersLanded gives.
func lyricsLanded(r waxlabel.TransferReport) bool {
	for _, it := range r.Items {
		if it.Kind == waxlabel.TransferSyncedLyric &&
			(it.Disposition == waxlabel.Carried || it.Disposition == waxlabel.Lossy) {
			return true
		}
	}
	return false
}

// lyricsDropNote reports lines whose instants the cut removed. setsDropped of
// origSets is how many whole sets were emptied and removed with them: "1 line
// dropped" reads very differently when it was the only line a language had,
// and a document with several languages must say which fraction of them went.
func lyricsDropNote(dropped, setsDropped, origSets int) string {
	note := fmt.Sprintf("%d synced lyric lines pointed at removed audio and were dropped", dropped)
	if dropped == 1 {
		note = "1 synced lyric line pointed at removed audio and was dropped"
	}
	switch {
	case setsDropped == 0:
	case setsDropped == origSets && origSets == 1:
		note += "; the set was dropped"
	case setsDropped == origSets:
		note += fmt.Sprintf("; all %d sets were dropped", origSets)
	default:
		note += fmt.Sprintf("; %d of %d sets were dropped", setsDropped, origSets)
	}
	return note
}

// restoreOwnAudio writes back the own-audio tags the transfer excluded. It
// exists for the whole-file remux case only, where the output's packets are
// the input's and the excluded values still describe them exactly. It returns
// the keys it put back.
func restoreOwnAudio(ctx context.Context, doc *waxlabel.Document, path string, src *waxlabel.Document, report waxlabel.TransferReport) ([]string, string, error) {
	var keys []tag.Key
	for _, it := range report.Items {
		if it.Kind == waxlabel.TransferField && it.Disposition == waxlabel.Excluded {
			keys = append(keys, it.Key)
		}
	}
	if len(keys) == 0 {
		return nil, "", nil
	}
	d, err := docOrParse(ctx, doc, path)
	if err != nil {
		return nil, "", err
	}
	ed := d.Edit()
	var restored []string
	for _, k := range keys {
		if vals, ok := src.Get(k); ok {
			ed.Set(k, vals...)
			restored = append(restored, string(k))
		}
	}
	plan, err := ed.Prepare()
	if err != nil {
		return nil, "", err
	}
	_, note, err := executeSaveBack(ctx, plan)
	if err != nil {
		return nil, "", err
	}
	return restored, note, nil
}
