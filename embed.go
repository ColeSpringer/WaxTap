package waxtap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/iox"
	"github.com/colespringer/waxtap/v3/youtube"
)

// maxThumbnailBytes caps a fetched thumbnail. YouTube cover-art JPEGs are well
// under this; the cap guards against a hostile or mistaken response.
const maxThumbnailBytes = 16 << 20

// embedOptions selects which parts of the metadata post-pass run.
type embedOptions struct {
	thumbnail bool
	metadata  bool
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
// unobtainable image) but the audio and tags were still written; a returned error
// means the file could not be edited at all. The contract is that any dropped
// picture is reported, never silent.
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

	if isMP4Path(path) {
		if err := remux("progressive"); err != nil {
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
		img, ferr := c.thumbnailImage(ctx, v)
		if ferr == nil {
			// MIME is left empty: AddPicture sniffs the bytes authoritatively, so a
			// JPEG/PNG/WebP thumbnail is stored under the MIME its bytes actually are.
			ed.AddPicture(waxlabel.Picture{Type: waxlabel.PicFrontCover, Data: img})
			changed = true
		} else if skipReason == "" {
			skipReason = "could not embed cover art: " + ferr.Error()
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
			ed.SetChapters(toWaxlabelChapters(v.Chapters)...)
			changed = true
		}
	}

	if changed {
		plan, perr := ed.Prepare()
		if perr != nil {
			return "", perr
		}
		if _, _, perr := plan.Execute(ctx, waxlabel.SaveBack()); perr != nil {
			return "", perr
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

// thumbnailImage fetches and validates the video's largest thumbnail. It returns
// a descriptive error when there is no thumbnail, the fetch fails, or the bytes
// are not a recognized image, so the caller can warn rather than drop silently.
func (c *Client) thumbnailImage(ctx context.Context, v *youtube.Video) ([]byte, error) {
	if len(v.Thumbnails) == 0 {
		return nil, errors.New("the video carries no thumbnail")
	}
	img, err := c.fetchThumbnail(ctx, v.Thumbnails[0].URL)
	if err != nil {
		return nil, err
	}
	if !waxlabel.IsRecognizedImage(img) {
		return nil, errors.New("the thumbnail is not a recognized image format")
	}
	return img, nil
}

// fetchThumbnail downloads a thumbnail image over the shared HTTP client, capped.
func (c *Client) fetchThumbnail(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thumbnail fetch: HTTP %d", resp.StatusCode)
	}
	return iox.ReadAllCapped(resp.Body, maxThumbnailBytes, "thumbnail")
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

// isMP4Path reports whether path names an MP4-family container, which WaxLabel
// can only tag in its progressive (non-fragmented) form.
func isMP4Path(path string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "m4a", "mp4", "m4b":
		return true
	}
	return false
}

// pictureCapableExt reports whether a file with this extension can hold cover art
// (directly, or after a remux to the codec's native container). It is false for
// the containers that cannot: WebM (the Matroska subset lacks Attachments), raw
// ADTS, WAV, and AIFF. An empty extension (a stream sink) is treated as capable:
// there is no filename to misname, so a remux to a picture-capable container is
// safe.
func pictureCapableExt(ext string) bool {
	switch strings.ToLower(ext) {
	case "webm", "aac", "wav", "aiff", "aif":
		return false
	}
	return true
}
