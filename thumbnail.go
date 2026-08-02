package waxtap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/colespringer/waxlabel"

	"github.com/colespringer/waxtap/v3/internal/coverart"
	"github.com/colespringer/waxtap/v3/internal/iox"
	"github.com/colespringer/waxtap/v3/youtube"
)

// maxThumbnailBytes caps a fetched thumbnail. YouTube cover-art JPEGs are well
// under this; the cap guards against a hostile or mistaken response.
const maxThumbnailBytes = 16 << 20

// maxThumbnailAttempts caps the candidate walk at two synthesized probes plus
// two listed rungs. The whole pass is best-effort and runs with the audio already
// on disk, so a miss costs a request, never the download.
const maxThumbnailAttempts = 4

// highResArea is the pixel area of the deterministic 1280x720 endpoints. A ladder
// that already reaches it is not worth probing.
const highResArea = 1280 * 720

// minProbeArea floors what a probe's response must beat. Without it the
// placeholder guard goes inert exactly where the probes are pure guesses: an
// empty ladder makes the best listed area 0, and every image beats 0.
//
// That is a live path, not a hypothetical. A WEB player-context extraction builds
// its Video with an ID, title, author, duration, and formats and no Thumbnails at
// all, and the full-metadata watch-page pass backfills the publish date,
// chapters, and availability but never the ladder. Before the probes existed that
// path reported "the video carries no thumbnail" and embedded nothing, which
// beats embedding a grey tile.
//
// 640x480 is the sddefault rung, the smallest image worth replacing a ladder
// with. It clears the 120x90 placeholder by a wide margin and sits far under a
// real render, including the 1278x720 some videos carry instead of a full 1280.
const minProbeArea = 640 * 480

// coverCandidate is one cover-art URL to try.
type coverCandidate struct {
	url string
	// probe marks a URL WaxTap synthesized rather than one YouTube listed. A
	// missing high-resolution rung does not always 404: i.ytimg.com also answers
	// 200 with a small grey placeholder, so a probe's response is only accepted
	// when its header dimensions beat minArea, the area of the best entry the
	// ladder actually listed. That is the exact reason the probe was issued, and
	// it catches the placeholder (10,800 pixels against sddefault's 307,200)
	// without a magic minimum width.
	probe   bool
	minArea int
}

// coverPicture fetches the cover art for v and shapes it per mode. It walks the
// candidates best-first and takes the first usable response, so a probe that
// misses costs one request rather than the picture.
//
// The returned note is non-empty when a picture was obtained but could not be
// shaped as asked; the bytes are still valid and still worth embedding. An error
// means no usable image was found at all.
func (c *Client) coverPicture(ctx context.Context, v *youtube.Video, mode CoverArtMode) ([]byte, string, error) {
	cands := thumbnailCandidates(v)
	if len(cands) == 0 {
		return nil, "", errors.New("the video carries no thumbnail")
	}

	var (
		img     []byte
		picked  coverCandidate
		lastErr error
		tried   int
	)
	for _, cand := range cands {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		tried++
		data, err := c.fetchThumbnail(ctx, cand.url)
		if err != nil {
			lastErr = err
			// The next candidate is the same host, so it would answer the same way
			// after another backoff.
			if errors.Is(err, ErrRateLimited) {
				break
			}
			continue
		}
		if !waxlabel.IsRecognizedImage(data) {
			lastErr = fmt.Errorf("%s: not a recognized image format", cand.url)
			continue
		}
		if cand.probe {
			w, h, derr := coverart.Dimensions(data)
			if derr != nil {
				lastErr = fmt.Errorf("%s: %w", cand.url, derr)
				continue
			}
			if w*h <= cand.minArea {
				lastErr = fmt.Errorf("%s: %dx%d is no larger than the listed ladder", cand.url, w, h)
				continue
			}
		}
		img, picked = data, cand
		break
	}
	if img == nil {
		// Report the last error, not the first: candidates are ordered best-first,
		// so the last one tried is the humblest and the most likely to exist.
		return nil, "", fmt.Errorf("tried %d thumbnail URLs, last: %w", tried, lastErr)
	}

	before := coverDims(img)
	note := ""
	if mode == CoverArtSquare {
		shaped, serr := coverart.Square(img)
		// Square never returns nil bytes: on failure it hands back its input along
		// with a report of the degraded result.
		img = shaped
		if serr != nil {
			note = fmt.Sprintf("embedded the cover art unshaped: %v", serr)
		}
	}
	c.log.DebugContext(ctx, "waxtap: embedding cover art",
		"url", picked.url, "probe", picked.probe, "before", before, "after", coverDims(img))
	return img, note, nil
}

// thumbnailCandidates returns the cover-art URLs to try, best first: the
// synthesized high-resolution probes (when the listed ladder falls short of
// them) followed by the ladder itself.
//
// The ladder is taken in slice order, which is largest-area-first: extraction is
// the only thing that ever fills Video.Thumbnails and it sorts in place before
// returning. That ordering is what makes truncating to maxThumbnailAttempts keep
// the best rungs rather than an arbitrary two, so re-sorting here would only
// repeat work the field's contract already guarantees.
func thumbnailCandidates(v *youtube.Video) []coverCandidate {
	best := bestListedArea(v.Thumbnails)
	var out []coverCandidate
	// A ladder that already reaches 1280x720 pays nothing. Listed area rather than
	// literal dimensions, so this is the same predicate as the placeholder guard.
	if best < highResArea {
		out = upgradeToHighRes(v, best)
	}
	for _, t := range v.Thumbnails {
		// A probe can name a URL the ladder also lists, when a rung declares a size
		// smaller than the endpoint it points at. Both would then answer the same
		// way, so the repeat would spend an attempt to learn nothing. The ladder's
		// own contents are YouTube's to change, unlike the ordering above.
		if containsURL(out, t.URL) {
			continue
		}
		out = append(out, coverCandidate{url: t.URL})
	}
	if len(out) > maxThumbnailAttempts {
		out = out[:maxThumbnailAttempts]
	}
	return out
}

// containsURL reports whether cs already names url. A linear scan beats a set for
// a list this short, capped at maxThumbnailAttempts plus the probes.
func containsURL(cs []coverCandidate, url string) bool {
	return slices.ContainsFunc(cs, func(c coverCandidate) bool { return c.url == url })
}

// upgradeToHighRes returns the deterministic 1280x720 endpoints for v.
//
// hq720 exists for sources that never got a maxres render and costs nothing when
// maxres lands, because it is only reached on the 404. Two probes plus the best
// listed rung is a better spend of the attempt budget than two listed rungs that
// differ by a factor of two.
//
// The probes deliberately do not go into Video.Thumbnails: that field is the
// record of what YouTube reported, and a library caller iterating it must not
// receive a URL WaxTap guessed.
func upgradeToHighRes(v *youtube.Video, listedArea int) []coverCandidate {
	id := coverVideoID(v)
	if id == "" {
		return nil
	}
	out := make([]coverCandidate, 0, 2)
	for _, name := range []string{"maxresdefault.jpg", "hq720.jpg"} {
		out = append(out, coverCandidate{
			url:   "https://i.ytimg.com/vi/" + id + "/" + name,
			probe: true,
			// Floored, so an empty ladder does not reduce the guard to "beat zero".
			minArea: max(listedArea, minProbeArea),
		})
	}
	return out
}

// coverVideoID returns the ID to build probe URLs from: the one a ytimg URL in
// the ladder already carries, else the video's own ID.
//
// Falling back to v.ID matters most where the probe is most valuable: an empty
// ladder otherwise yields "the video carries no thumbnail" and a guessed
// maxresdefault is pure upside. The "do not guess for a foreign ladder" worry
// only applies to a caller-fabricated Video, which cannot reach here, since the
// embed pass is called only from the YouTube source with its own extracted video.
func coverVideoID(v *youtube.Video) string {
	for _, t := range v.Thumbnails {
		if id := ytimgVideoID(t.URL); id != "" {
			return id
		}
	}
	if looksLikeVideoID(v.ID) {
		return v.ID
	}
	return ""
}

// ytimgVideoID extracts the video ID from an i.ytimg.com thumbnail URL, or
// returns "". Both the /vi/ and /vi_webp/ families are accepted; the caller
// rebuilds a plain /vi/ JPEG URL, which normalizes the family and drops the
// query (sqp and rs resize directives are signed for the size they were issued
// at, so they cannot be carried onto a different rung).
func ytimgVideoID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "ytimg.com" && !strings.HasSuffix(host, ".ytimg.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || (parts[0] != "vi" && parts[0] != "vi_webp") {
		return ""
	}
	if !looksLikeVideoID(parts[1]) {
		return ""
	}
	return parts[1]
}

// looksLikeVideoID reports whether s is shaped like a YouTube video ID: exactly
// 11 characters from the base64url alphabet.
func looksLikeVideoID(s string) bool {
	if len(s) != 11 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// bestListedArea returns the largest pixel area YouTube listed, or 0 for an
// empty or sizeless ladder.
func bestListedArea(ts []youtube.Thumbnail) int {
	best := 0
	for _, t := range ts {
		if t.Width > 0 && t.Height > 0 {
			best = max(best, t.Width*t.Height)
		}
	}
	return best
}

// coverDims renders an image's pixel size for a log line, or "unknown" for bytes
// the stdlib cannot read a header from (a WebP thumbnail, most likely).
func coverDims(data []byte) string {
	w, h, err := coverart.Dimensions(data)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%dx%d", w, h)
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
		// Name the URL: the walk reports one cause for several candidates, so a
		// bare status would not say which rung answered it.
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return iox.ReadAllCapped(resp.Body, maxThumbnailBytes, "thumbnail")
}
