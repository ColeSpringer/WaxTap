package sponsorblock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v2/internal/cache"
	"github.com/colespringer/waxtap/v2/internal/httpx"
	"github.com/colespringer/waxtap/v2/internal/iox"
	"github.com/colespringer/waxtap/v2/waxerr"
)

const (
	// DefaultBaseURL is the public SponsorBlock API endpoint. It is configurable so
	// deployments can point at a mirror or self-hosted instance.
	DefaultBaseURL = "https://sponsor.ajay.app"

	// hashPrefixLen is how many leading SHA-256 hex characters of the video ID we
	// send. The privacy endpoint accepts 4-32; 4 keeps the exact video ID private
	// while bounding the response size.
	hashPrefixLen = 4

	// maxResponseBytes bounds the buffered response. A 4-char prefix can match
	// many videos, so the array is filtered client-side down to the exact ID.
	maxResponseBytes = 8 << 20 // 8 MiB

	// userAgent identifies WaxTap to the SponsorBlock server.
	userAgent = "WaxTap (+https://github.com/colespringer/waxtap)"

	// skipActionType is the only action WaxTap honors. Mute/poi/chapter/full
	// actions do not describe a removable span.
	skipActionType = "skip"

	// defaultCacheTTL is how long a hash-prefix response is reused. Segment data
	// changes by community submissions, so a short TTL avoids repeated prefix
	// fetches without hiding updates for long.
	defaultCacheTTL = 30 * time.Minute

	// standaloneMaxRetries keeps the self-built HTTP client conservative. The
	// facade injects its shared client in normal use.
	standaloneMaxRetries = 2
)

// Segment is a single SponsorBlock skip segment for one video.
type Segment struct {
	// Category is the SponsorBlock category assigned to the segment.
	Category Category
	// ActionType is retained from the API response. FetchSegments currently
	// returns only "skip" segments.
	ActionType string
	// Start and End are the half-open time span to remove.
	Start time.Duration
	End   time.Duration // exclusive end offset
	// UUID is the SponsorBlock submission ID.
	UUID string
	// Locked and Votes are the SponsorBlock authority signals used to choose
	// between overlapping submissions.
	Locked bool
	Votes  int // net community vote count
	// VideoDuration is 0 when the server did not report it.
	VideoDuration time.Duration
}

// Client fetches SponsorBlock segments. It is safe for concurrent use.
//
// It uses the privacy hash-prefix endpoint, caches prefix responses by category
// set, and coalesces concurrent identical requests. Per-host rate limiting comes
// from the injected httpx.Client.
type Client struct {
	http    *httpx.Client
	baseURL string
	log     *slog.Logger
	// cache maps a hash-prefix request to the parsed per-video segment lists. It
	// is nil when caching is disabled.
	cache *cache.Store[map[string][]Segment]
}

// Config configures a Client. The zero value is usable.
type Config struct {
	// HTTP is the retrying HTTP client. If nil, a conservative default is built.
	HTTP *httpx.Client
	// BaseURL overrides the API base URL. Empty uses DefaultBaseURL.
	BaseURL string
	// Logger receives debug logs. Nil discards them.
	Logger *slog.Logger
	// CacheTTL is how long a hash-prefix response is reused. Zero uses
	// defaultCacheTTL; a negative value disables caching (and singleflight).
	CacheTTL time.Duration
}

// NormalizeBaseURL cleans a SponsorBlock base URL for concatenation: it removes
// surrounding whitespace (which would fail http.NewRequest) and a trailing slash
// (which would produce a "//api/skipSegments" path some servers 404 or redirect).
// It is the single normalization the client and the WaxTap facade both apply, so
// the validated value and the fetched value agree. It does not validate the scheme
// or host.
func NormalizeBaseURL(base string) string {
	return strings.TrimSuffix(strings.TrimSpace(base), "/")
}

// New returns a Client, filling unset Config fields with defaults. BaseURL is
// normalized via NormalizeBaseURL so endpoint builds a clean URL regardless of the
// caller.
func New(cfg Config) *Client {
	c := &Client{
		http:    cfg.HTTP,
		baseURL: NormalizeBaseURL(cfg.BaseURL),
		log:     cfg.Logger,
	}
	if c.http == nil {
		c.http = httpx.New(httpx.Config{MaxRetries: standaloneMaxRetries})
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if cfg.CacheTTL >= 0 {
		ttl := cfg.CacheTTL
		if ttl == 0 {
			ttl = defaultCacheTTL
		}
		c.cache = cache.NewStore[map[string][]Segment](cache.Options{TTL: ttl})
	}
	return c
}

// FetchSegments returns skip segments for videoID in the requested categories.
// It queries the privacy hash-prefix endpoint, filters the response to the exact
// video ID and skip-action segments, then keeps the best overlapping submission
// per region: locked first, then most-voted.
//
// Responses are cached per hash prefix and category set. One prefix response can
// serve every matching video for the cache lifetime.
//
// A 404 means the prefix has no segments and returns an empty slice, not an
// error. An empty categories slice falls back to DefaultCategories.
//
// FetchSegments does not set its own timeout; callers should pass a bounded
// context and decide whether failures are fatal.
func (c *Client) FetchSegments(ctx context.Context, videoID string, categories []Category) ([]Segment, error) {
	if videoID == "" {
		return nil, fmt.Errorf("sponsorblock: empty video id")
	}
	if len(categories) == 0 {
		categories = DefaultCategories
	}

	byVideo, err := c.fetchPrefix(ctx, videoID, categories)
	if err != nil {
		return nil, err
	}
	return byVideo[videoID], nil
}

// fetchPrefix returns the parsed per-video segment lists for videoID's hash
// prefix, served from cache when possible and otherwise loaded once (shared
// across concurrent callers). The cache key is the prefix plus the category set,
// since the server filters by category.
func (c *Client) fetchPrefix(ctx context.Context, videoID string, categories []Category) (map[string][]Segment, error) {
	wanted := categorySet(categories)
	load := func(ctx context.Context) (map[string][]Segment, error) {
		body, status, err := c.get(ctx, videoID, categories)
		if err != nil {
			return nil, err
		}
		switch status {
		case http.StatusOK:
			return parseResponse(body, wanted)
		case http.StatusNotFound:
			return map[string][]Segment{}, nil // no segments for this prefix
		default:
			return nil, &waxerr.HTTPStatusError{StatusCode: status, URL: c.baseURL}
		}
	}

	if c.cache == nil {
		return load(ctx)
	}
	return c.cache.GetOrLoad(ctx, c.cacheKey(videoID, categories), load)
}

// cacheKey is the hash prefix plus the sorted category set. The prefix (not the
// video ID) is the key so different videos sharing it reuse one response.
func (c *Client) cacheKey(videoID string, categories []Category) string {
	cats := categoryStrings(categories)
	sort.Strings(cats)
	return hashPrefix(videoID) + "|" + strings.Join(cats, ",")
}

// get performs the hash-prefix request and returns the body, status, and any
// transport/limit error. A non-2xx status is returned to the caller (which
// classifies 404 vs. other), not turned into an error here.
func (c *Client) get(ctx context.Context, videoID string, categories []Category) (body []byte, status int, err error) {
	endpoint, err := c.endpoint(videoID, categories)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	b, err := iox.ReadAllCapped(resp.Body, maxResponseBytes, "SponsorBlock response")
	if err != nil {
		return nil, 0, err
	}
	return b, resp.StatusCode, nil
}

// endpoint builds the privacy hash-prefix URL for videoID with the category and
// action-type filters in the query string.
func (c *Client) endpoint(videoID string, categories []Category) (string, error) {
	catsJSON, err := json.Marshal(categoryStrings(categories))
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("categories", string(catsJSON))
	q.Set("actionTypes", `["`+skipActionType+`"]`)
	return c.baseURL + "/api/skipSegments/" + hashPrefix(videoID) + "?" + q.Encode(), nil
}

// hashPrefix returns the leading hex characters of the video ID's SHA-256.
func hashPrefix(videoID string) string {
	sum := sha256.Sum256([]byte(videoID))
	return hex.EncodeToString(sum[:])[:hashPrefixLen]
}

// apiVideo is one element of the hash-prefix response array: all segments for a
// single video ID whose hash starts with the requested prefix.
type apiVideo struct {
	VideoID  string       `json:"videoID"`
	Segments []apiSegment `json:"segments"`
}

// apiSegment mirrors a SponsorBlock segment. segment is [start, end] in seconds;
// locked is 0/1.
type apiSegment struct {
	Category      string     `json:"category"`
	ActionType    string     `json:"actionType"`
	Segment       [2]float64 `json:"segment"`
	UUID          string     `json:"UUID"`
	Locked        int        `json:"locked"`
	Votes         int        `json:"votes"`
	VideoDuration float64    `json:"videoDuration"`
}

// parseResponse parses the hash-prefix array into per-video segment lists,
// keeping skip-action segments in the wanted categories and collapsing
// overlapping submissions. The result keys videos by exact ID, so callers index
// the one they asked for (and any other video sharing the prefix is cached too).
func parseResponse(body []byte, wanted map[Category]bool) (map[string][]Segment, error) {
	var videos []apiVideo
	if err := json.Unmarshal(body, &videos); err != nil {
		return nil, fmt.Errorf("sponsorblock: parse response: %w", err)
	}
	out := make(map[string][]Segment, len(videos))
	for _, v := range videos {
		if segs := segmentsOf(v, wanted); len(segs) > 0 {
			out[v.VideoID] = bestPerOverlap(segs)
		}
	}
	return out, nil
}

// segmentsOf converts one video's wanted, skip-action, non-empty segments.
func segmentsOf(v apiVideo, wanted map[Category]bool) []Segment {
	var out []Segment
	for _, s := range v.Segments {
		if s.ActionType != skipActionType {
			continue
		}
		cat := Category(s.Category)
		if !wanted[cat] {
			continue
		}
		if s.Segment[1] <= s.Segment[0] {
			continue // zero or negative length
		}
		out = append(out, Segment{
			Category:      cat,
			ActionType:    s.ActionType,
			Start:         secondsToDuration(s.Segment[0]),
			End:           secondsToDuration(s.Segment[1]),
			UUID:          s.UUID,
			Locked:        s.Locked != 0,
			Votes:         s.Votes,
			VideoDuration: secondsToDuration(s.VideoDuration),
		})
	}
	return out
}

// bestPerOverlap collapses overlapping submissions to one segment per region,
// preferring locked segments, then higher vote totals. Touching but
// non-overlapping segments (end == next start) are kept separate, so genuinely
// distinct adjacent regions both survive.
func bestPerOverlap(segs []Segment) []Segment {
	if len(segs) <= 1 {
		return segs
	}
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].Start != segs[j].Start {
			return segs[i].Start < segs[j].Start
		}
		return segs[i].End < segs[j].End
	})

	var out []Segment
	cluster := []Segment{segs[0]}
	clusterEnd := segs[0].End
	flush := func() { out = append(out, bestOf(cluster)) }

	for _, s := range segs[1:] {
		if s.Start < clusterEnd { // overlaps the current cluster
			cluster = append(cluster, s)
			clusterEnd = max(clusterEnd, s.End)
			continue
		}
		flush()
		cluster = []Segment{s}
		clusterEnd = s.End
	}
	flush()
	return out
}

func bestOf(cluster []Segment) Segment {
	best := cluster[0]
	for _, s := range cluster[1:] {
		if betterSegment(s, best) {
			best = s
		}
	}
	return best
}

// betterSegment reports whether a is a more authoritative submission than b: a
// locked segment beats an unlocked one, then more votes win.
func betterSegment(a, b Segment) bool {
	if a.Locked != b.Locked {
		return a.Locked
	}
	return a.Votes > b.Votes
}

func categorySet(categories []Category) map[Category]bool {
	set := make(map[Category]bool, len(categories))
	for _, c := range categories {
		set[c] = true
	}
	return set
}

func categoryStrings(categories []Category) []string {
	out := make([]string, len(categories))
	for i, c := range categories {
		out[i] = string(c)
	}
	return out
}

// secondsToDuration converts SponsorBlock's float seconds to a time.Duration
// without losing sub-second precision.
func secondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
