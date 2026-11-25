package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/youtube"
)

// embedOptions selects which parts of the metadata post-pass run.
type embedOptions struct {
	thumbnail bool
	metadata  bool
	coverArt  CoverArtMode
	// cut is the timeline cut the pipeline applied, nil when none ran. Chapters
	// are remapped through it so their offsets match the delivered audio.
	cut *appliedCut
}

// embedRequested reports whether the spec asks for any embed post-pass.
func embedRequested(s ProcessSpec) bool {
	return s.EmbedThumbnail || s.EmbedMetadata
}

// embedMetadata runs the opt-in cover-art / tag post-pass on a written output
// file. It is best-effort: a failure or a skipped picture emits a warning and
// leaves valid audio, never failing the download. targetExt is the extension the
// delivered file will carry (empty for a stream sink), so the post-pass never
// remuxes into a container the extension would then misname.
//
// dest is the path warnings name. The pass runs on a staged or job-temp file
// whose name the user never asked for and will never see, so warnings report
// the destination instead; "" falls back to path.
// It returns the container extension the pass left the file in when it remuxed
// for the picture ("" when it did not), so the caller can report the delivered
// format as what is really on disk rather than as the source's.
func (c *Client) embedMetadata(ctx context.Context, path, dest, targetExt string, v *youtube.Video, o embedOptions, em *emitter) (remuxedTo string) {
	if v == nil || (!o.thumbnail && !o.metadata) {
		return ""
	}
	remuxedTo, skipReason, err := c.doEmbed(ctx, path, targetExt, v, o)
	if err != nil {
		em.warn(WarnMetadataEmbed, fmt.Sprintf("could not embed metadata into %s: %v", warnName(dest, path), err))
		return ""
	}
	if skipReason != "" {
		em.warn(WarnMetadataEmbed, skipReason)
	}
	return remuxedTo
}

// doEmbed performs the WaxLabel edit. It returns a non-empty skipReason when a
// requested cover picture could not be embedded (an unusable container, or an
// unobtainable image), or could not be shaped as asked, or when a step after a
// committed write failed, but the audio and tags were still written; a returned
// error means the file could not be edited at all. The contract is that any
// dropped or degraded outcome is reported, never silent.
//
// Two container fix-ups may run first, both a zero-re-encode packet copy and both
// staged so the original file is replaced only after every step succeeds:
//   - a fragmented MP4 is flattened to progressive (WaxLabel refuses fragmented MP4);
//   - when a picture is wanted but the container cannot hold one (WebM, the Matroska
//     subset YouTube ships, carries tags but not cover art) and the delivered
//     extension can carry a picture, the audio is remuxed to its codec's native
//     container (Opus-in-WebM to Ogg-Opus), which can.
func (c *Client) doEmbed(ctx context.Context, path, targetExt string, v *youtube.Video, o embedOptions) (remuxedTo, skipReason string, err error) {
	// work is the file the edits run against; while it differs from path it is a
	// scratch copy, so a mid-flight failure leaves the original untouched.
	work := path
	scratch := ""
	scratchSeq := 0
	committed := false
	defer func() {
		if scratch != "" && !committed {
			_ = os.Remove(scratch)
		}
	}()

	remux := func(container string) error {
		runner := c.engine()
		next := embedScratchPath(path, scratchSeq)
		scratchSeq++
		if rerr := runner.RemuxContainer(ctx, work, next, container); rerr != nil {
			return rerr
		}
		if scratch != "" {
			_ = os.Remove(scratch)
		}
		scratch, work = next, next
		return nil
	}

	if c.isMP4File(ctx, path) {
		if err := remux(media.ContainerProgressive); err != nil {
			return "", "", fmt.Errorf("flatten MP4 for tagging: %w", err)
		}
	}

	doc, err := waxlabel.ParseFile(ctx, work)
	if err != nil {
		return "", "", err
	}
	caps := doc.Capabilities()

	if o.thumbnail && caps.Pictures.Write == waxlabel.AccessNone {
		if pictureCapableExt(targetExt) {
			// Remux to the codec's native container (lossless) so the picture can be
			// written and the delivered extension still matches the content. A remux
			// or re-parse failure is fatal to the edit, not a silent skip.
			if err := remux(""); err != nil {
				return "", "", fmt.Errorf("remux for cover art: %w", err)
			}
			d2, perr := waxlabel.ParseFile(ctx, work)
			if perr != nil {
				return "", "", perr
			}
			doc, caps = d2, d2.Capabilities()
			// The file is now in its codec's native container, which is not
			// the one the source arrived in, so the caller reports the
			// delivered format from this: "webm" would name a file that no
			// longer exists. The MP4 flatten above is not one of these, since
			// it stays in the same container family.
			//
			// targetExt is the answer, and it needs no probe to find: the
			// remux runs only when pictureCapableExt(targetExt) holds, and
			// that is precisely the test that the codec's native container is
			// the one this extension names. It is also the extension the file
			// is delivered under, so nothing here can name a file that is not
			// on disk.
			remuxedTo = strings.ToLower(strings.TrimPrefix(targetExt, "."))
		} else {
			// A remux would leave the content mismatched with the target extension
			// (e.g. Ogg bytes in a .webm file). Skip the picture and report it.
			skipReason = fmt.Sprintf("cover art cannot be written to a .%s file; deliver as .opus/.flac/.mp3/.m4a or pass --format to embed it", targetExt)
		}
	}

	ed := doc.Edit()
	changed := false

	if o.thumbnail && caps.Pictures.Write != waxlabel.AccessNone {
		img, note, ferr := c.coverPicture(ctx, v, o.coverArt)
		switch {
		case ferr != nil:
			if skipReason == "" {
				skipReason = "could not embed cover art: " + ferr.Error()
			}
		default:
			// MIME is left empty: AddPicture sniffs the bytes authoritatively, so a
			// JPEG/PNG/WebP thumbnail is stored under the MIME its bytes actually are.
			ed.AddPicture(waxlabel.Picture{Type: waxlabel.PicFrontCover, Data: img})
			changed = true
			// The picture landed but not in the shape asked for. No collision risk
			// with the block above: that one only runs when Pictures.Write is
			// AccessNone, which this branch requires not to be.
			if note != "" {
				skipReason = note
			}
		}
	}

	if o.metadata {
		// Each tag is written only when the video has a value for it, never as
		// an empty or zero one: what a field holds depends on the client that
		// answered and on what the video states (see tagsFor).
		t := tagsFor(v)
		for _, f := range []struct {
			key  tag.Key
			vals []string
		}{
			{tag.Title, nonEmpty(t.title)},
			{tag.Artist, t.artists},
			{tag.Album, nonEmpty(t.album)},
			{tag.RecordingDate, nonEmpty(t.year)},
			{tag.Copyright, nonEmpty(t.copyright)},
		} {
			if len(f.vals) > 0 {
				ed.Set(f.key, f.vals...)
				changed = true
			}
		}
		if len(v.Chapters) > 0 {
			if caps.Chapters.Write == waxlabel.AccessNone {
				skipReason = joinSkip(skipReason, fmt.Sprintf("chapters cannot be embedded in a %s file", caps.Format))
			} else {
				chs := toWaxlabelChapters(v.Chapters)
				if o.cut != nil {
					chs = remapChapters(chs, o.cut)
				}
				if len(chs) > 0 {
					ed.SetChapters(chs...)
					changed = true
				}
			}
		}
	}

	if changed {
		plan, perr := ed.Prepare()
		if perr != nil {
			return "", "", perr
		}
		_, note, perr := executeSaveBack(ctx, plan)
		if perr != nil {
			return "", "", perr
		}
		if note != "" {
			skipReason = joinSkip(skipReason, "metadata written, but "+note)
		}
	}

	// Publish the scratch (remux + tags) to the original path, atomically. This runs
	// even when only a remux happened (no tags): a container fix-up is still the
	// delivered content, matching the extension the caller named.
	if scratch != "" {
		if err := os.Rename(scratch, path); err != nil {
			return "", "", err
		}
		committed = true
	}
	return remuxedTo, skipReason, nil
}

// embedTags is the text EmbedMetadata writes for a video. An empty field is a
// tag left unwritten.
type embedTags struct {
	title     string
	artists   []string
	album     string
	year      string // YYYY
	copyright string
}

// topicSuffix ends the name of an auto-generated artist channel, the kind Art
// Tracks are uploaded to. YouTube appends it to the artist's name; it is not
// part of one.
const topicSuffix = " - Topic"

// tagsFor derives the tags from what the video states, best effort and
// without looking anything up.
//
// The title is the video's own, with one exception: an Art Track's is the one
// its description states, which is the same title except that it is whole
// where YouTube cut the video's short. The Music card is no such exception.
// It may name a shorter title than the video's, which says more (a featured
// credit, a version), so the video's is not rewritten from it.
//
// The artists, album, and copyright line are the ones v.Music states. A video
// that states none is credited to its channel, which is the best YouTube
// offers, less the suffix of an auto-generated one.
func tagsFor(v *youtube.Video) embedTags {
	t := embedTags{title: v.Title, year: tagYear(v)}
	if m := v.Music; m != nil {
		if m.Source == youtube.MusicFromDescription {
			t.title = m.Title
		}
		t.artists, t.album, t.copyright = m.Artists, m.Album, m.Copyright
		return t
	}
	t.artists = nonEmpty(strings.TrimSpace(strings.TrimSuffix(v.Author, topicSuffix)))
	return t
}

// tagYear picks the year to file the video's audio under, the most specific
// claim first:
//
//   - the year of the release date the video states;
//   - the year of its copyright line, when that is no later than the year it
//     was uploaded. Catalog is delivered to YouTube years after it came out,
//     so there the upload dates the delivery and the copyright year, though
//     it can be a remaster's, is the nearer one;
//   - the year it was uploaded, which for a new release is the release year
//     and for anything else is the only date there is.
//
// It returns "" when the video carries none of them, as on the default client
// without a watch-page pass.
//
// A year is all the tag takes. Only one of the three sources knows the
// release day, and the upload's day is the delivery's; a caller that wants
// more has Music.ReleaseDate and Video.PublishDate in full.
func tagYear(v *youtube.Video) string {
	uploaded := v.PublishDate.Year()
	if m := v.Music; m != nil {
		if !m.ReleaseDate.IsZero() {
			return strconv.Itoa(m.ReleaseDate.Year())
		}
		// With no upload date to hold the year against, this year bounds it:
		// a later one is a label's name, not a date.
		latest := uploaded
		if v.PublishDate.IsZero() {
			latest = time.Now().Year()
		}
		if y := copyrightYear(m.Copyright); y != 0 && y <= latest {
			return strconv.Itoa(y)
		}
	}
	if v.PublishDate.IsZero() {
		return ""
	}
	return strconv.Itoa(uploaded)
}

// copyrightYear returns the year a copyright line opens with, or 0 when it
// opens with anything else. Only the leading number counts ("1981, 2017" is
// 1981, the first publication), and only as a plausible year standing alone:
// labels are named for numbers too.
func copyrightYear(line string) int {
	rest := strings.TrimLeft(line, "\u2117\u00a9 ")
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits != 4 {
		return 0
	}
	y, _ := strconv.Atoi(rest[:4])
	if y < 1900 {
		return 0
	}
	return y
}

// nonEmpty returns s as a one-value list, or nil when it is empty.
func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// executeSaveBack runs plan against its parsed file in place, applying
// WaxLabel's committed-write contract: an error with SaveResult.Committed true
// means the bytes landed and only a step after the commit failed (e.g. the
// directory fsync); the plan is spent and retrying is refused, so that is a
// success carrying a warning note, not a failure. The note also relays the
// plan's discard warnings, WaxLabel's record of edit content the write threw
// away rather than stored, so a write-time drop no caps gate or transfer
// report anticipated still reaches the user. The returned document is the
// post-write one, nil only when nothing was written (err non-nil).
func executeSaveBack(ctx context.Context, plan *waxlabel.Plan) (doc *waxlabel.Document, note string, err error) {
	doc, sr, err := plan.Execute(ctx, waxlabel.SaveBack())
	switch {
	case err == nil:
		return doc, discardNotes(plan.Report().Warnings), nil
	case !sr.Committed:
		return nil, "", err
	default:
		return doc, joinSkip(discardNotes(plan.Report().Warnings), "a post-write step failed: "+err.Error()), nil
	}
}

// discardNotes renders the discard warnings in ws as one note, or "" when
// there are none. Coercion and advisory warnings stay silent: the caps gates
// and the transfer report already tell the per-item story, and this is the
// backstop for content thrown away at write time.
func discardNotes(ws []waxlabel.Warning) string {
	var msgs []string
	for _, w := range ws {
		if waxlabel.IsDiscardWarning(w.Code) {
			msgs = append(msgs, w.Message)
		}
	}
	const maxMsgs = 3
	if len(msgs) > maxMsgs {
		msgs = append(msgs[:maxMsgs], fmt.Sprintf("and %d more", len(msgs)-maxMsgs))
	}
	return strings.Join(msgs, "; ")
}

// joinSkip appends a report to a possibly empty predecessor.
func joinSkip(prev, next string) string {
	if prev == "" {
		return next
	}
	return prev + "; " + next
}

// toWaxlabelChapters maps youtube chapters to waxlabel chapters. Both use
// time.Duration and treat a zero End as "until the next chapter".
func toWaxlabelChapters(chs []youtube.Chapter) []waxlabel.Chapter {
	out := make([]waxlabel.Chapter, len(chs))
	for i, ch := range chs {
		out[i] = waxlabel.Chapter{Start: ch.Start, End: ch.End, Title: ch.Title}
	}
	return out
}

// embedScratchPath returns a distinct temp path next to path (same directory, for
// an atomic rename) for staging remux number seq during embedding. The sequence
// keeps successive remuxes on separate paths, so a later stage never reads and
// writes the file a prior stage produced.
func embedScratchPath(path string, seq int) string {
	return filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.embed%d", filepath.Base(path), seq))
}

// isMP4File reports whether path holds an MP4-family container, which WaxLabel
// can only tag in its progressive (non-fragmented) form. It sniffs instead of
// reading the extension because every AAC/ALAC encode is MP4 whatever the output
// is named, and a keep-source download is never container-checked, so even
// -o out.flac can hold the source's AAC-in-MP4 bytes.
//
// A failed probe falls back to the extension, the rule this replaced. The embed
// pass only warns on error, so reporting false here would cost the user their
// tags without failing the download.
//
// pipeline.Result.OutputProbe carries the same Format.Container but is nil for a
// keep-source download, so it cannot serve this. The comparison is exact because
// WaxFlow reports one registry name per container, and "mp4" covers m4a through
// mov.
func (c *Client) isMP4File(ctx context.Context, path string) bool {
	pr, err := c.engine().Probe(ctx, path)
	if err != nil {
		return isMP4Ext(path)
	}
	return pr.Format.Container == "mp4"
}

// isMP4Ext reports whether path is named like an MP4-family container. It serves
// only as isMP4File's fallback for a file that will not probe.
func isMP4Ext(path string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "m4a", "mp4", "m4b", "m4r", "mov":
		return true
	}
	return false
}

// pictureCapableExt reports the extensions that must not be remuxed into another
// container to gain cover art. WebM is the case that matters: the Matroska subset
// YouTube ships has no Attachments, and remuxing to Ogg would leave the bytes
// misnamed. Raw ADTS is the same. WAV and AIFF are kept as a floor but never
// reached, since WaxLabel writes pictures into both and the caller only consults
// this function when Pictures.Write is AccessNone.
//
// An empty extension (a stream sink) counts as capable: there is no filename to
// misname, so the remux is safe.
func pictureCapableExt(ext string) bool {
	ext = strings.ToLower(ext)
	if media.IsWAVExt(ext) || media.IsAIFFExt(ext) {
		// All of PCM's spellings answer alike, so a keep-source delivery
		// under .wave is judged as one under .wav is.
		return false
	}
	switch ext {
	case "webm", "aac":
		return false
	case "wv", "ape", "mka", "mkv":
		// This switch is keyed on the DELIVERED extension, not the work file's
		// format: a keep-source download can put Opus-in-WebM bytes under any
		// of these names, and the remux would leave Ogg bytes misnamed. A
		// genuine WavPack, APE, or Matroska file never consults this function
		// (its Pictures.Write is not AccessNone), so these entries exist for
		// the mismatched keep-source deliveries only.
		return false
	}
	if _, decodeOnly := media.DecodeOnlyContainer(ext); decodeOnly {
		// The same mismatch under a name WaxTap only reads, and nothing could be
		// written under it in any case.
		return false
	}
	return true
}

// remuxedFormat reports f as the file a remux left behind: ext is the
// extension it was delivered under, and the MIME type follows the codec in
// that container. Everything else about the format is the delivery's own,
// because a remux moves packets and changes nothing about the audio. An empty
// ext (no remux ran) returns f unchanged.
//
// The cover-art pass is one caller; a container copy the output extension
// asked for is the other, and both leave the same file: the delivery's
// packets in a wrapper the delivery did not name.
func remuxedFormat(f Format, ext string) Format {
	if ext == "" {
		return f
	}
	// The container changed, so the byte count no longer describes the file:
	// a remux rewrites the wrapper around the same packets. Result.OutputBytes
	// carries the delivered size, so clearing this leaves one true answer
	// rather than two that disagree. The codec's own rate is unchanged, since
	// the packets are.
	f.ContentLength = 0
	// The delivered extension as the caller named it, never a canonical
	// spelling of the codec's: this has to name the file that is on disk.
	f.Extension = strings.ToLower(strings.TrimPrefix(ext, "."))
	switch f.Extension {
	case "opus", "ogg", "oga":
		if strings.EqualFold(f.Codec, "opus") {
			f.MIMEType = `audio/ogg; codecs="opus"`
		} else {
			f.MIMEType = `audio/ogg; codecs="vorbis"`
		}
	case "mka", "mkv":
		// Matroska states no codecs parameter the way the Ogg types do, so
		// the codec stays on Format.Codec alone.
		f.MIMEType = "audio/x-matroska"
	case "m4a", "m4b", "mp4":
		// The MP4 family, which a delivery of its own already names this
		// way: an .m4a copied to .m4b is the same container under another
		// name, so the type it arrived with is the type it keeps, codecs
		// parameter included.
		f.MIMEType = "audio/mp4"
		if f.Codec != "" {
			f.MIMEType += `; codecs="` + f.Codec + `"`
		}
	case "aac":
		f.MIMEType = "audio/aac" // raw ADTS, which states no codecs parameter
	default:
		f.MIMEType = ""
	}
	return f
}
