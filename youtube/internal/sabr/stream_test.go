package sabr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

func baseConfig(d HTTPDoer) Config {
	return Config{
		HTTP:            d,
		ServerAbrURL:    "https://sabr.example/initplayback?key=v",
		UstreamerConfig: []byte("ustreamer"),
		Format:          FormatId{Itag: 251, LastModified: 1700000000000001},
		ClientInfo:      ClientInfo{ClientName: 1, ClientVersion: "2.x", OSName: "Windows", OSVersion: "10.0", AcceptLanguage: "en-US"},
		UserAgent:       "Mozilla/5.0 test",
		POToken:         []byte("gvs-pot"),
		ContentLength:   10,
		RoundTimeout:    5 * time.Second,
	}
}

// initFrames builds the UMP parts for an init segment carrying data.
func initFrames(headerID uint32, data []byte) []byte {
	return concat(
		umpFrame(partMediaHeader, marshalMediaHeader(MediaHeader{HeaderID: headerID, Itag: 251, IsInitSeg: true})),
		mediaFrame(headerID, data),
	)
}

// segFrames builds the UMP parts for one media segment.
func segFrames(headerID uint32, seq uint64, data []byte) []byte {
	return concat(
		umpFrame(partMediaHeader, marshalMediaHeader(MediaHeader{HeaderID: headerID, Itag: 251, SequenceNumber: seq, DurationMs: 5000})),
		mediaFrame(headerID, data),
	)
}

func formatInitFrame(endSegment int64) []byte {
	return umpFrame(partFormatInitMetadata, marshalFormatInit(FormatInitializationMetadata{FormatId: FormatId{Itag: 251}, EndSegmentNumber: endSegment, MimeType: `audio/webm; codecs="opus"`}))
}

func TestOpenHappyPath(t *testing.T) {
	resp := concat(
		umpFrame(partStreamProtection, marshalStreamProtectionStatus(StreamProtectionStatus{Status: 1})), // OK, not a signal
		initFrames(0, []byte("INIT")),
		segFrames(1, 1, []byte("AAA")),
		segFrames(2, 2, []byte("BBB")),
		formatInitFrame(2),
		umpFrame(partNextRequestPolicy, marshalNextRequestPolicy(NextRequestPolicy{PlaybackCookie: []byte("c1")})),
	)
	d := &scriptedDoer{t: t, responses: [][]byte{resp}}

	var progress []Progress
	rc, info, err := Open(context.Background(), baseConfig(d), func(p Progress) { progress = append(progress, p) })
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB", got)
	}
	if d.calls != 1 {
		t.Errorf("calls = %d, want 1; whole stream fits in one round", d.calls)
	}
	if info.ContentLength != 10 {
		t.Errorf("StreamInfo.ContentLength = %d, want 10 (seeded from config)", info.ContentLength)
	}
	if info.ContentType != `audio/webm; codecs="opus"` {
		t.Errorf("StreamInfo.ContentType = %q, want the format init mime type", info.ContentType)
	}
	if len(progress) == 0 {
		t.Fatal("no progress reported")
	}
	last := progress[len(progress)-1]
	if last.BytesWritten != 10 || last.Total != 10 {
		t.Errorf("final progress = %+v, want {10 10}", last)
	}

	// The request carried the selected itag, ustreamer config, and GVS PO token.
	req := protoScan(t, d.bodies[0])
	if got := string(one(t, req, 5).b); got != "ustreamer" {
		t.Errorf("request ustreamer config = %q", got)
	}
	fid := protoScan(t, req[2][0].b)
	if got := one(t, fid, 1).v; got != 251 {
		t.Errorf("request selected itag = %d, want 251", got)
	}
	sc := protoScan(t, one(t, req, 19).b)
	if got := string(one(t, sc, 2).b); got != "gvs-pot" {
		t.Errorf("request po_token = %q, want gvs-pot", got)
	}
}

func TestOpenMultiRoundThreadsCookieAndBufferedRange(t *testing.T) {
	resp1 := concat(
		initFrames(0, []byte("INIT")),
		segFrames(1, 1, []byte("AAA")),
		formatInitFrame(2),
		umpFrame(partNextRequestPolicy, marshalNextRequestPolicy(NextRequestPolicy{BackoffTimeMs: 1, PlaybackCookie: []byte("cookie-1")})),
	)
	resp2 := segFrames(9, 2, []byte("BBB"))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, resp2}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB", got)
	}
	if d.calls != 2 {
		t.Fatalf("calls = %d, want 2", d.calls)
	}

	// The second request must echo the playback cookie and report a buffered range
	// covering segment 1.
	req2 := protoScan(t, d.bodies[1])
	sc := protoScan(t, one(t, req2, 19).b)
	if got := string(one(t, sc, 3).b); got != "cookie-1" {
		t.Errorf("request2 playback_cookie = %q, want cookie-1", got)
	}
	if len(req2[3]) != 1 {
		t.Fatalf("request2 buffered_ranges = %d, want 1", len(req2[3]))
	}
	br := protoScan(t, req2[3][0].b)
	if got := one(t, br, 5).v; got != 1 { // end_segment_index
		t.Errorf("request2 buffered_range end_segment_index = %d, want 1", got)
	}
}

func TestOpenFollowsRedirectWithDescramble(t *testing.T) {
	const rawRedirect = "https://r2.example/videoplayback?n=RAWN"
	const solved = "https://r2.example/videoplayback?n=SOLVED"

	resp1 := umpFrame(partSabrRedirect, marshalSabrRedirect(SabrRedirect{URL: rawRedirect}))
	resp2 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(1))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, resp2}}

	cfg := baseConfig(d)
	var descrambled string
	cfg.DescrambleN = func(_ context.Context, rawURL string) (string, error) {
		descrambled = rawURL
		return solved, nil
	}

	rc, _, err := Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "INITAAA" {
		t.Errorf("stream = %q, want INITAAA", got)
	}
	if descrambled != rawRedirect {
		t.Errorf("DescrambleN got %q, want %q", descrambled, rawRedirect)
	}
	if d.calls != 2 {
		t.Fatalf("calls = %d, want 2", d.calls)
	}
	if d.urls[1] != solved {
		t.Errorf("redirect followed to %q, want %q", d.urls[1], solved)
	}
}

func TestOpenClampsServerBackoff(t *testing.T) {
	resp1 := concat(
		initFrames(0, []byte("INIT")),
		segFrames(1, 1, []byte("AAA")),
		formatInitFrame(2),
		umpFrame(partNextRequestPolicy, marshalNextRequestPolicy(NextRequestPolicy{BackoffTimeMs: 3_600_000})), // 1h
	)
	resp2 := segFrames(2, 2, []byte("BBB"))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, resp2}}

	cfg := baseConfig(d)
	cfg.MaxBackoff = 30 * time.Millisecond

	start := time.Now()
	rc, _, err := Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 20*time.Millisecond {
		t.Errorf("elapsed %v, backoff not honored (want >= ~30ms)", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("elapsed %v, backoff not clamped (server asked for 1h)", elapsed)
	}
}

func TestOpenAttestationRequired(t *testing.T) {
	resp := umpFrame(partStreamProtection, marshalStreamProtectionStatus(StreamProtectionStatus{Status: 3, MaxRetries: 2}))
	d := &scriptedDoer{t: t, responses: [][]byte{resp}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Fatalf("err = %v, want ErrNeedsPOToken", err)
	}
}

func TestOpenSabrError(t *testing.T) {
	resp := umpFrame(partSabrError, marshalSabrError(SabrError{Type: "sabr.test_error", Code: 17}))
	d := &scriptedDoer{t: t, responses: [][]byte{resp}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed", err)
	}
	if !strings.Contains(err.Error(), "sabr.test_error") || !strings.Contains(err.Error(), "code=17") {
		t.Errorf("err = %q, want SABR type and code", err)
	}
}

func TestOpenReloadPlayer(t *testing.T) {
	resp := umpFrame(partReloadPlayerResp, nil)
	d := &scriptedDoer{t: t, responses: [][]byte{resp}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, ErrReloadPlayer) {
		t.Fatalf("err = %v, want ErrReloadPlayer", err)
	}
}

func TestOpenStallBeforeFinalSegmentIsError(t *testing.T) {
	// The stream advertises three segments but stops after the first. Returning
	// EOF here would silently truncate the download.
	resp1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(3))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, nil, nil}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (stalled)", err)
	}
}

// policyFrame is a NextRequestPolicy-only round (server pacing, no media).
func policyFrame(backoffMs int64) []byte {
	return umpFrame(partNextRequestPolicy, marshalNextRequestPolicy(NextRequestPolicy{BackoffTimeMs: backoffMs}))
}

func TestOpenTruncationWithKnownLengthIsError(t *testing.T) {
	// No formatInitFrame is sent, so only the content length reveals that the
	// seven-byte response is incomplete.
	resp1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, nil, nil}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (truncated below content length)", err)
	}
}

func TestOpenUnknownLengthCleanEOF(t *testing.T) {
	resp1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, nil, nil}}

	cfg := baseConfig(d)
	cfg.ContentLength = 0 // unknown: nothing to compare against
	rc, _, err := Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read = %v, want clean EOF when length is unknown", err)
	}
	if string(got) != "INITAAA" {
		t.Errorf("stream = %q, want INITAAA", got)
	}
}

func TestOpenMissingInitSegmentIsError(t *testing.T) {
	resp1 := concat(segFrames(1, 1, []byte("AAA")), formatInitFrame(1)) // seq 1, end 1, no init
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, nil, nil}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (missing init segment)", err)
	}
	if err != nil && !strings.Contains(err.Error(), "init segment") {
		t.Errorf("err = %q, want it to mention the missing init segment", err)
	}
}

func TestOpenInitAfterMediaKeepsOrder(t *testing.T) {
	resp1 := segFrames(2, 1, []byte("AAA"))                            // media first, no init yet
	resp2 := concat(initFrames(0, []byte("INIT")), formatInitFrame(1)) // init arrives later
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, resp2}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "INITAAA" {
		t.Errorf("stream = %q, want INITAAA (init must precede media)", got)
	}
}

func TestOpenBackoffRoundsDoNotStall(t *testing.T) {
	resp1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(2), policyFrame(1))
	// Policy-only rounds exceed maxEmptyRounds but should not count as stalls.
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, policyFrame(1), policyFrame(1), segFrames(2, 2, []byte("BBB"))}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read = %v, want success across backoff rounds", err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB", got)
	}
	if d.calls != 4 {
		t.Errorf("calls = %d, want 4 (initial + 2 backoff + final)", d.calls)
	}
}

func TestOpenHTTPErrorStatus(t *testing.T) {
	d := &scriptedDoer{t: t, responses: [][]byte{nil}, statuses: []int{403}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	var httpErr *waxerr.HTTPStatusError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want *waxerr.HTTPStatusError", err)
	}
	if httpErr.StatusCode != 403 {
		t.Errorf("status = %d, want 403", httpErr.StatusCode)
	}
}

func TestOpenContextCanceled(t *testing.T) {
	d := doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("transport should not be hit when context is already canceled")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := Open(ctx, baseConfig(d), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
