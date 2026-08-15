package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
func (c *Client) embedMetadata(ctx context.Context, path, targetExt string, v *youtube.Video, o embedOptions, em *emitter) {
	if v == nil || (!o.thumbnail && !o.metadata) {
		return
	}
	skipReason, err := c.doEmbed(ctx, path, targetExt, v, o)
	if err != nil {
		em.warn(WarnMetadataEmbed, fmt.Sprintf("could not embed metadata into %s: %v", filepath.Base(path), err))
		return
	}
	if skipReason != "" {
		em.warn(WarnMetadataEmbed, skipReason)
	}
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
func (c *Client) doEmbed(ctx context.Context, path, targetExt string, v *youtube.Video, o embedOptions) (skipReason string, err error) {
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
			return "", fmt.Errorf("flatten MP4 for tagging: %w", err)
		}
	}

	doc, err := waxlabel.ParseFile(ctx, work)
	if err != nil {
		return "", err
	}
	caps := doc.Capabilities()

	if o.thumbnail && caps.Pictures.Write == waxlabel.AccessNone {
		if pictureCapableExt(targetExt) {
			// Remux to the codec's native container (lossless) so the picture can be
			// written and the delivered extension still matches the content. A remux
			// or re-parse failure is fatal to the edit, not a silent skip.
			if err := remux(""); err != nil {
				return "", fmt.Errorf("remux for cover art: %w", err)
			}
			d2, perr := waxlabel.ParseFile(ctx, work)
			if perr != nil {
				return "", perr
			}
			doc, caps = d2, d2.Capabilities()
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
		// Title and artist come from VideoDetails and are always present. Date and
		// chapters are WEB/mobile-shaped and often absent on the default path; skip
		// them when missing rather than stamping a zero value.
		if v.Title != "" {
			ed.Set(tag.Title, v.Title)
			changed = true
		}
		if v.Author != "" {
			// YouTube exposes channel metadata, not an authoritative artist, so the
			// channel name is deliberately mapped to ARTIST.
			ed.Set(tag.Artist, v.Author)
			changed = true
		}
		if !v.PublishDate.IsZero() {
			ed.Set(tag.RecordingDate, v.PublishDate.Format("2006-01-02"))
			changed = true
		}
		if len(v.Chapters) > 0 && caps.Chapters.Write != waxlabel.AccessNone {
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

	if changed {
		plan, perr := ed.Prepare()
		if perr != nil {
			return "", perr
		}
		_, note, perr := executeSaveBack(ctx, plan)
		if perr != nil {
			return "", perr
		}
		if note != "" {
			note = "metadata written, but " + note
			if skipReason != "" {
				skipReason += "; " + note
			} else {
				skipReason = note
			}
		}
	}

	// Publish the scratch (remux + tags) to the original path, atomically. This runs
	// even when only a remux happened (no tags): a container fix-up is still the
	// delivered content, matching the extension the caller named.
	if scratch != "" {
		if err := os.Rename(scratch, path); err != nil {
			return "", err
		}
		committed = true
	}
	return skipReason, nil
}

// executeSaveBack runs plan against its parsed file in place, applying
// WaxLabel's committed-write contract: an error with SaveResult.Committed true
// means the bytes landed and only a step after the commit failed (e.g. the
// directory fsync); the plan is spent and retrying is refused, so that is a
// success carrying a warning note, not a failure. The returned document is the
// post-write one, nil only when nothing was written (err non-nil).
func executeSaveBack(ctx context.Context, plan *waxlabel.Plan) (doc *waxlabel.Document, note string, err error) {
	doc, sr, err := plan.Execute(ctx, waxlabel.SaveBack())
	if err == nil {
		return doc, "", nil
	}
	if !sr.Committed {
		return nil, "", err
	}
	return doc, "a post-write step failed: " + err.Error(), nil
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
	switch strings.ToLower(ext) {
	case "webm", "aac", "wav", "aiff", "aif", "aifc", "afc":
		return false
	}
	return true
}
