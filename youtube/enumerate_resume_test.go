package youtube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// resumeWorld serves a seven-entry playlist in pages of 3, 3, and 1, the later
// two under the tokens TOK_B and TOK_C. asked records what each browse request
// named: "first", or the token it continued from.
type resumeWorld struct {
	t      *testing.T
	asked  []string
	status map[string]int    // per page, an HTTP status to answer instead
	body   map[string]string // per page, a body to answer instead, under status when both are set
	first  map[string]string // per page, a body for its first request alone
	hold   map[string]bool   // per page, hold the request until its context ends
	oembed int               // oEmbed's status for a refused page
	onAsk  func(page string) // called as each request arrives, "oembed" for oEmbed's
}

func resumeID(n int) string { return fmt.Sprintf("resumeVid%02d", n) }

// resumeItems renders entries from..to as legacy renderers, then the marker
// for next when there is one.
func resumeItems(from, to int, next string) string {
	var items []string
	for n := from; n <= to; n++ {
		items = append(items, fmt.Sprintf(`{"playlistVideoRenderer": {"videoId": %q, "title": {"runs": [{"text": "Song %d"}]}, "lengthSeconds": "180"}}`, resumeID(n), n))
	}
	if next != "" {
		items = append(items, fmt.Sprintf(`{"continuationItemRenderer": {"continuationEndpoint": {"continuationCommand": {"token": %q}}}}`, next))
	}
	return strings.Join(items, ",")
}

// resumeHeader is what WEB states on every page, a continuation included: the
// title, and a sidebar with the count and the owner.
const resumeHeader = `"metadata": {"playlistMetadataRenderer": {"title": "Resume Test"}},
	"sidebar": {"playlistSidebarRenderer": {"items": [
		{"playlistSidebarPrimaryInfoRenderer": {"stats": [{"runs": [{"text": "7"}, {"text": " videos"}]}]}},
		{"playlistSidebarSecondaryInfoRenderer": {"videoOwner": {"videoOwnerRenderer": {"title": {"runs": [{"text": "Owner"}]}}}}}
	]}}`

func (w *resumeWorld) page(name string) string {
	switch name {
	case "first":
		return `{` + resumeHeader + `, "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [{"tabRenderer": {"content": {"sectionListRenderer": {"contents": [{"itemSectionRenderer": {"contents": [{"playlistVideoListRenderer": {"contents": [` + resumeItems(1, 3, "TOK_B") + `]}}]}}]}}}}]}}}`
	case "TOK_B":
		return `{` + resumeHeader + `, "onResponseReceivedActions": [{"appendContinuationItemsAction": {"continuationItems": [` + resumeItems(4, 6, "TOK_C") + `]}}]}`
	case "TOK_C":
		return `{` + resumeHeader + `, "onResponseReceivedActions": [{"appendContinuationItemsAction": {"continuationItems": [` + resumeItems(7, 7, "") + `]}}]}`
	}
	w.t.Errorf("browse asked for an unknown page %q", name)
	return `{}`
}

func (w *resumeWorld) client() *Client {
	return newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oembed" {
			if w.onAsk != nil {
				w.onAsk("oembed")
			}
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			return fixtureResp(w.oembed, nil), nil
		}
		if !strings.HasSuffix(r.URL.Path, "/browse") {
			w.t.Errorf("unexpected request: %s", r.URL)
			return fixtureResp(http.StatusNotFound, nil), nil
		}
		raw, _ := readAll(r)
		var req struct {
			BrowseID     string `json:"browseId"`
			Continuation string `json:"continuation"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			w.t.Errorf("browse body: %v", err)
		}
		name := req.Continuation
		if name == "" {
			if req.BrowseID != "VLPLtest" {
				w.t.Errorf("browseId = %q, want VLPLtest", req.BrowseID)
			}
			name = "first"
		}
		w.asked = append(w.asked, name)
		if w.onAsk != nil {
			w.onAsk(name)
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if w.hold[name] {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		if b, ok := w.first[name]; ok {
			delete(w.first, name)
			return fixtureResp(http.StatusOK, []byte(b)), nil
		}
		b, hasBody := w.body[name]
		if s := w.status[name]; s != 0 {
			if !hasBody {
				b = `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`
			}
			return fixtureResp(s, []byte(b)), nil
		}
		if hasBody {
			return fixtureResp(http.StatusOK, []byte(b)), nil
		}
		return fixtureResp(http.StatusOK, []byte(w.page(name))), nil
	}))
}

// listed is an entry reduced to what a resumed listing must get right.
type listed struct {
	ID    string
	Index int
}

func listedOf(entries []PlaylistEntry) []listed {
	out := make([]listed, len(entries))
	for i, e := range entries {
		out[i] = listed{e.VideoID, e.Index}
	}
	return out
}

// stitch lists PLtest in calls capped at maxItems, each resuming from the last
// one's Continuation, until a call returns none.
func stitch(t *testing.T, w *resumeWorld, maxItems int, skip func(string) bool) []listed {
	t.Helper()
	var got []listed
	token := ""
	for call := 1; ; call++ {
		if call > 10 {
			t.Fatalf("cap %d: still resuming after %d calls", maxItems, call-1)
		}
		pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: maxItems, Skip: skip, Continuation: token})
		if err != nil {
			t.Fatalf("cap %d, call %d: %v", maxItems, call, err)
		}
		if len(pl.Errors) > 0 {
			t.Fatalf("cap %d, call %d: errors %v", maxItems, call, pl.Errors)
		}
		got = append(got, listedOf(pl.Entries)...)
		if pl.Continuation == "" {
			return got
		}
		token = pl.Continuation
	}
}

// Whatever the cap, in a page or at its edge, the capped calls together list
// what one uncapped call does, each entry at its true position.
func TestEnumerateResume_AnyCapStitchesTheFullListing(t *testing.T) {
	skipTwo := func(id string) bool { return id == resumeID(2) || id == resumeID(5) }
	for name, skip := range map[string]func(string) bool{"plain": nil, "with Skip": skipTwo} {
		t.Run(name, func(t *testing.T) {
			want := stitch(t, &resumeWorld{t: t}, 0, skip)
			if n := 7; skip == nil && len(want) != n {
				t.Fatalf("uncapped listing has %d entries, want %d", len(want), n)
			}
			for maxItems := 1; maxItems <= 8; maxItems++ {
				if got := stitch(t, &resumeWorld{t: t}, maxItems, skip); !slices.Equal(got, want) {
					t.Errorf("cap %d stitched\n %v, want\n %v", maxItems, got, want)
				}
			}
		})
	}
}

// A cap that lands on a page's edge resumes from the next page alone; one that
// lands inside a page reads that page again and drops what was listed.
func TestEnumerateResume_ReadsOnlyThePagesItNeeds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxItems int
		want     []string // the resumed call's requests
	}{
		{"page edge", 3, []string{"TOK_B", "TOK_C"}},
		{"inside the first page", 2, []string{"first", "TOK_B", "TOK_C"}},
		{"inside a later page", 5, []string{"TOK_B", "TOK_C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &resumeWorld{t: t}
			pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: tc.maxItems})
			if err != nil {
				t.Fatal(err)
			}
			if pl.Continuation == "" {
				t.Fatal("Continuation is empty after a capped listing with entries left")
			}
			w.asked = nil
			rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(w.asked, tc.want) {
				t.Errorf("resumed requests = %v, want %v", w.asked, tc.want)
			}
			if got, want := len(rest.Entries), 7-tc.maxItems; got != want {
				t.Errorf("resumed entries = %d, want %d", got, want)
			}
			if rest.Continuation != "" {
				t.Errorf("Continuation = %q after the last page, want empty", rest.Continuation)
			}
		})
	}
}

// A resumed call starts on a later page and reads the header that page states.
func TestEnumerateResume_ReadsTheHeaderOfItsFirstPage(t *testing.T) {
	w := &resumeWorld{t: t}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
	if err != nil {
		t.Fatal(err)
	}
	w.asked = nil
	rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(w.asked, "first") {
		t.Fatalf("resumed requests = %v, want the first page left unread", w.asked)
	}
	if rest.ID != "PLtest" || rest.Title != "Resume Test" || rest.Author != "Owner" || rest.VideoCount != 7 {
		t.Errorf("resumed playlist = {%q %q %q %d}, want {PLtest, Resume Test, Owner, 7}", rest.ID, rest.Title, rest.Author, rest.VideoCount)
	}
}

// A page that fails ends the listing with its error and a Continuation that
// names that page, so the next call asks for it again.
func TestEnumerateResume_AfterAFailedPage(t *testing.T) {
	w := &resumeWorld{t: t, status: map[string]int{"TOK_B": http.StatusForbidden}}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 3 || len(pl.Errors) != 1 {
		t.Fatalf("got %d entries and %d errors, want 3 and 1", len(pl.Entries), len(pl.Errors))
	}
	if pl.Continuation == "" {
		t.Fatal("Continuation is empty after a failed page")
	}
	w.status, w.asked = nil, nil
	rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
	if err != nil {
		t.Fatal(err)
	}
	want := []listed{{resumeID(4), 3}, {resumeID(5), 4}, {resumeID(6), 5}, {resumeID(7), 6}}
	if got := listedOf(rest.Entries); !slices.Equal(got, want) {
		t.Errorf("resumed entries = %v, want %v", got, want)
	}
	if !slices.Equal(w.asked, []string{"TOK_B", "TOK_C"}) {
		t.Errorf("resumed requests = %v, want [TOK_B TOK_C]", w.asked)
	}
}

func resumeToken(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// A Continuation Enumerate did not return for this playlist is refused before
// any request.
func TestEnumerateResume_RejectsAForeignContinuation(t *testing.T) {
	w := &resumeWorld{t: t}
	other, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
	if err != nil {
		t.Fatal(err)
	}
	w.asked = nil
	for name, tc := range map[string]struct{ playlist, token string }{
		"another playlist's": {"PLother", other.Continuation},
		"not base64":         {"PLtest", "TOK_B%3D"},
		"not a resume point": {"PLtest", base64.RawURLEncoding.EncodeToString([]byte("TOK_B"))},
		"no playlist":        {"PLtest", resumeToken(t, map[string]any{"t": "TOK_B"})},
		"negative offset":    {"PLtest", resumeToken(t, map[string]any{"p": "PLtest", "t": "TOK_B", "o": -1})},
		"negative index":     {"PLtest", resumeToken(t, map[string]any{"p": "PLtest", "t": "TOK_B", "i": -1})},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.client().Enumerate(context.Background(), tc.playlist, EnumOptions{Continuation: tc.token})
			if !errors.Is(err, waxerr.ErrInvalidContinuation) {
				t.Errorf("err = %v, want ErrInvalidContinuation", err)
			}
		})
	}
	if len(w.asked) != 0 {
		t.Errorf("requests = %v, want none", w.asked)
	}
}

// YouTube answers a token it cannot read with HTTP 400. Any other status is
// the request's failure, and the token may still be good.
func TestEnumerateResume_YouTubeRefusesTheToken(t *testing.T) {
	w := &resumeWorld{t: t}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
	if err != nil {
		t.Fatal(err)
	}

	w.status = map[string]int{"TOK_B": http.StatusBadRequest}
	rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
	if !errors.Is(err, waxerr.ErrInvalidContinuation) || rest != nil {
		t.Errorf("HTTP 400: got (%v, %v), want (nil, ErrInvalidContinuation)", rest, err)
	}

	w.status = map[string]int{"TOK_B": http.StatusForbidden}
	rest, err = w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
	if hse, ok := errors.AsType[*waxerr.HTTPStatusError](err); !ok || hse.StatusCode != http.StatusForbidden || rest != nil {
		t.Errorf("HTTP 403: got (%v, %v), want (nil, the HTTP 403)", rest, err)
	}
	if errors.Is(err, waxerr.ErrInvalidContinuation) {
		t.Errorf("HTTP 403: err = %v, want it not ErrInvalidContinuation", err)
	}
}

// preconditionFailed is YouTube's other HTTP 400, measured 2026-11: it refuses
// the client context (a missing one, a retired client version), whatever the
// ID or token beside it.
const preconditionFailed = `{"error":{"code":400,"message":"Precondition check failed.","status":"FAILED_PRECONDITION"}}`

// A 400 that refuses the client says nothing about the token or the ID, so it
// stays an HTTP error: told its token is bad, a caller would discard a good
// one and list again into the same refusal.
func TestEnumerate_PreconditionFailureIsNotABadTokenOrID(t *testing.T) {
	w := &resumeWorld{t: t}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ page, continuation string }{
		"resumed":    {"TOK_B", pl.Continuation},
		"first page": {"first", ""},
	} {
		t.Run(name, func(t *testing.T) {
			w.status = map[string]int{tc.page: http.StatusBadRequest}
			w.body = map[string]string{tc.page: preconditionFailed}
			_, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: tc.continuation})
			if hse, ok := errors.AsType[*waxerr.HTTPStatusError](err); !ok || hse.StatusCode != http.StatusBadRequest {
				t.Fatalf("err = %v, want the HTTP 400", err)
			}
			if errors.Is(err, waxerr.ErrInvalidContinuation) || errors.Is(err, waxerr.ErrInvalidPlaylistID) {
				t.Errorf("err = %v, want neither ErrInvalidContinuation nor ErrInvalidPlaylistID", err)
			}
		})
	}
}

// A playlist made private or deleted since answers a continuation with the
// alert its first page gives, and the resumed call reports the refusal. A
// continuation never carries the page shell, so the kind is oEmbed's.
func TestEnumerateResume_PlaylistRefusedSince(t *testing.T) {
	for _, tc := range []struct {
		oembed int
		want   waxerr.PlaylistUnavailableKind
	}{
		{http.StatusForbidden, waxerr.PlaylistUnavailablePrivate},
		{http.StatusNotFound, waxerr.PlaylistUnavailableNotFound},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			w := &resumeWorld{t: t, oembed: tc.oembed}
			pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
			if err != nil {
				t.Fatal(err)
			}
			w.body = map[string]string{"TOK_B": alertPage("The playlist does not exist.", false)}
			_, err = w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
			pu, ok := errors.AsType[*waxerr.PlaylistUnavailableError](err)
			if !ok {
				t.Fatalf("err = %v, want *PlaylistUnavailableError", err)
			}
			if pu.Kind != tc.want || pu.Reason != "The playlist does not exist." {
				t.Errorf("refusal = {%v %q}, want {%v, the alert text}", pu.Kind, pu.Reason, tc.want)
			}
		})
	}
}

// The same alert on a later page of one listing keeps the entries gathered and
// reports the refusal among Errors, where it once read as a stale parser.
func TestEnumerate_PlaylistRefusedOnALaterPage(t *testing.T) {
	w := &resumeWorld{t: t, oembed: http.StatusForbidden,
		body: map[string]string{"TOK_B": alertPage("The playlist does not exist.", false)}}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 3 || len(pl.Errors) != 1 {
		t.Fatalf("got %d entries and %d errors, want 3 and 1", len(pl.Entries), len(pl.Errors))
	}
	pu, ok := errors.AsType[*waxerr.PlaylistUnavailableError](pl.Errors[0])
	if !ok || pu.Kind != waxerr.PlaylistUnavailablePrivate {
		t.Errorf("Errors[0] = %v, want a private *PlaylistUnavailableError", pl.Errors[0])
	}
}

// A cancellation that lands in a later page's request, or in the oEmbed lookup
// that classifies its refusal, is the call's error, as one that lands between
// pages is, and not a page failure among Errors.
func TestEnumerate_CancelDuringALaterPage(t *testing.T) {
	refused := map[string]string{"TOK_B": alertPage("The playlist does not exist.", false)}
	for _, tc := range []struct {
		name, at string
		body     map[string]string
	}{
		{"the page's request", "TOK_B", nil},
		{"its refusal's lookup", "oembed", refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &resumeWorld{t: t, oembed: http.StatusForbidden, body: tc.body, onAsk: func(asked string) {
				if asked == tc.at {
					cancel()
				}
			}}
			_, err := w.client().Enumerate(ctx, "PLtest", EnumOptions{})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		})
	}
}

// pastTheEnd is what YouTube answers a token at or past a playlist's end,
// measured 2026-11: the header, and no action to append anything.
const pastTheEnd = `{` + resumeHeader + `, "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [{"tabRenderer": {"selected": true}}]}}}`

// A playlist that shrank since its token was issued answers that token with a
// page that lists nothing. That is the end of the listing, on a resumed call
// and in the middle of one, and not a page the parser failed to read.
func TestEnumerateResume_TokenPastTheEnd(t *testing.T) {
	w := &resumeWorld{t: t}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
	if err != nil {
		t.Fatal(err)
	}
	w.body = map[string]string{"TOK_B": pastTheEnd}

	rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
	if err != nil {
		t.Fatalf("resumed: %v", err)
	}
	if len(rest.Entries) != 0 || len(rest.Errors) != 0 || rest.Continuation != "" || rest.Title != "Resume Test" {
		t.Errorf("resumed = %d entries, errors %v, Continuation %q, title %q; want an empty end of the list under its title",
			len(rest.Entries), rest.Errors, rest.Continuation, rest.Title)
	}

	whole, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{})
	if err != nil {
		t.Fatalf("one listing: %v", err)
	}
	if len(whole.Entries) != 3 || len(whole.Errors) != 0 || whole.Continuation != "" {
		t.Errorf("one listing = %d entries, errors %v, Continuation %q; want 3 and a clean end",
			len(whole.Entries), whole.Errors, whole.Continuation)
	}
}

// A resumed call's first page gets the retry the playlist's first page gets:
// a layout the parser does not know can be one request's experiment.
func TestEnumerateResume_RetriesAnUnrecognizedFirstPage(t *testing.T) {
	w := &resumeWorld{t: t}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
	if err != nil {
		t.Fatal(err)
	}
	w.asked = nil
	w.first = map[string]string{"TOK_B": `{"onResponseReceivedActions": [{"reloadContinuationItemsCommand": {}}]}`}
	rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Entries) != 4 || !slices.Equal(w.asked, []string{"TOK_B", "TOK_B", "TOK_C"}) {
		t.Errorf("got %d entries over requests %v, want 4 over [TOK_B TOK_B TOK_C]", len(rest.Entries), w.asked)
	}
}

// A deadline that runs out on a later page, in its request or before it, ends
// the listing as a failed page does: the entries gathered come back with the
// error and a Continuation. Only a cancellation is the call's own error.
func TestEnumerate_DeadlineOnALaterPageKeepsTheListing(t *testing.T) {
	for _, during := range []string{"the request", "the wait before it"} {
		t.Run(during, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			w := &resumeWorld{t: t}
			var o EnumOptions
			if during == "the request" {
				w.hold = map[string]bool{"TOK_B": true}
			} else {
				o.OnPage = func(int) { <-ctx.Done() }
			}
			pl, err := w.client().Enumerate(ctx, "PLtest", o)
			if err != nil {
				t.Fatalf("err = %v, want the listing so far", err)
			}
			if len(pl.Entries) != 3 || len(pl.Errors) != 1 || !errors.Is(pl.Errors[0], context.DeadlineExceeded) {
				t.Fatalf("got %d entries and errors %v, want 3 and the deadline", len(pl.Entries), pl.Errors)
			}
			w.hold = nil
			rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
			if err != nil {
				t.Fatalf("resumed: %v", err)
			}
			if len(rest.Entries) != 4 || rest.Entries[0].VideoID != resumeID(4) {
				t.Errorf("resumed = %v, want the four entries from %s", listedOf(rest.Entries), resumeID(4))
			}
		})
	}
}

// A later page that answers 404 is the refusal a first page's 404 is.
func TestEnumerate_NotFoundOnALaterPage(t *testing.T) {
	w := &resumeWorld{t: t, oembed: http.StatusNotFound, status: map[string]int{"TOK_B": http.StatusNotFound}}
	pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 3 || len(pl.Errors) != 1 {
		t.Fatalf("got %d entries and %d errors, want 3 and 1", len(pl.Entries), len(pl.Errors))
	}
	pu, ok := errors.AsType[*waxerr.PlaylistUnavailableError](pl.Errors[0])
	if !ok || pu.Kind != waxerr.PlaylistUnavailableNotFound {
		t.Errorf("Errors[0] = %v, want a not-found *PlaylistUnavailableError", pl.Errors[0])
	}
}

// notAVideo is a lockup for something other than a video, which a page can
// carry among its entries and which lists as an error.
const notAVideo = `{"lockupViewModel": {"contentId": "PLsomeOtherList", "contentType": "LOCKUP_CONTENT_TYPE_PLAYLIST"}}`

func resumeContinuation(items string) string {
	return `{"onResponseReceivedActions": [{"appendContinuationItemsAction": {"continuationItems": [` + items + `]}}]}`
}

// Continuation says entries are left. Items past the cap that are no videos
// leave none, so the point moves to the next page, or there is none.
func TestEnumerateResume_NoPointAtATailWithoutVideos(t *testing.T) {
	t.Run("last page", func(t *testing.T) {
		w := &resumeWorld{t: t, body: map[string]string{"TOK_C": resumeContinuation(resumeItems(7, 7, "") + "," + notAVideo)}}
		pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 7})
		if err != nil {
			t.Fatal(err)
		}
		if len(pl.Entries) != 7 || pl.Continuation != "" {
			t.Errorf("got %d entries and Continuation %q, want 7 and none", len(pl.Entries), pl.Continuation)
		}
	})
	t.Run("a page with one after it", func(t *testing.T) {
		w := &resumeWorld{t: t, body: map[string]string{"TOK_B": resumeContinuation(resumeItems(4, 6, "") + "," + notAVideo + "," + resumeItems(1, 0, "TOK_C"))}}
		pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 6})
		if err != nil {
			t.Fatal(err)
		}
		w.asked = nil
		rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(w.asked, []string{"TOK_C"}) || len(rest.Entries) != 1 || len(rest.Errors) != 0 {
			t.Errorf("resumed over %v: %d entries, errors %v; want [TOK_C], 1, none", w.asked, len(rest.Entries), rest.Errors)
		}
	})
}

func resumeFirstPage(items string) string {
	return `{` + resumeHeader + `, "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [{"tabRenderer": {"content": {"sectionListRenderer": {"contents": [{"itemSectionRenderer": {"contents": [{"playlistVideoListRenderer": {"contents": [` + items + `]}}]}}]}}}}]}}}`
}

func resumeVideo(id string) string {
	return fmt.Sprintf(`{"playlistVideoRenderer": {"videoId": %q, "title": {"runs": [{"text": "Song"}]}, "lengthSeconds": "180"}}`, id)
}

// A point inside a page is the last entry listed, found again when the page
// is re-read, so that what the page gained or lost above it neither repeats
// nor skips an entry. The stored offset stands when that entry is gone.
func TestEnumerateResume_FindsItsPlaceInAChangedPage(t *testing.T) {
	marker := resumeItems(1, 0, "TOK_B")
	for _, tc := range []struct {
		name   string
		reread string // the first page as the resumed call finds it
		want   string // the first entry it lists
	}{
		{"a new entry above", resumeVideo("resumeVidNew") + "," + resumeItems(1, 3, "TOK_B"), resumeID(3)},
		{"a non-video item above", notAVideo + "," + resumeItems(1, 3, "TOK_B"), resumeID(3)},
		{"an entry above removed", resumeItems(2, 3, "TOK_B"), resumeID(3)},
		{"the last entry listed removed", resumeVideo(resumeID(1)) + "," + resumeVideo(resumeID(3)) + "," + marker, resumeID(4)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &resumeWorld{t: t}
			pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 2})
			if err != nil {
				t.Fatal(err)
			}
			w.body = map[string]string{"first": resumeFirstPage(tc.reread)}
			rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
			if err != nil {
				t.Fatal(err)
			}
			if len(rest.Errors) != 0 || len(rest.Entries) == 0 || rest.Entries[0].VideoID != tc.want || rest.Entries[0].Index != 2 {
				t.Errorf("resumed = %v with errors %v, want it to start at {%s 2}", listedOf(rest.Entries), rest.Errors, tc.want)
			}
		})
	}

	// A playlist can list one video twice, so the match nearest the stored
	// offset is the place, not the first.
	t.Run("a video listed twice", func(t *testing.T) {
		twice := resumeFirstPage(resumeVideo(resumeID(1)) + "," + resumeVideo(resumeID(2)) + "," + resumeVideo(resumeID(1)) + "," + resumeVideo(resumeID(3)))
		w := &resumeWorld{t: t, body: map[string]string{"first": twice}}
		pl, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 3})
		if err != nil {
			t.Fatal(err)
		}
		rest, err := w.client().Enumerate(context.Background(), "PLtest", EnumOptions{Continuation: pl.Continuation})
		if err != nil {
			t.Fatal(err)
		}
		if want := []listed{{resumeID(3), 3}}; !slices.Equal(listedOf(rest.Entries), want) {
			t.Errorf("resumed = %v, want %v", listedOf(rest.Entries), want)
		}
	})
}
