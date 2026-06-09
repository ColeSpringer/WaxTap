package sabr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

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

	// The request carried the selected itag in selected_audio_format_ids (16),
	// the ustreamer config, and the GVS PO token.
	req := protoScan(t, d.bodies[0])
	if got := string(one(t, req, 5).b); got != "ustreamer" {
		t.Errorf("request ustreamer config = %q", got)
	}
	fid := protoScan(t, req[16][0].b)
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

// TestOpenAttestationPendingNoMetadataIsError covers a status-2 (PENDING) stream
// that ends without an end-segment or a content length: it may be a withheld
// partial, so it must error rather than be served as complete.
func TestOpenAttestationPendingNoMetadataIsError(t *testing.T) {
	resp := concat(
		umpFrame(partStreamProtection, marshalStreamProtectionStatus(StreamProtectionStatus{Status: 2})),
		initFrames(0, []byte("INIT")),
		segFrames(1, 1, []byte("AAA")),
		// no formatInitFrame -> endSegment stays 0
	)
	d := &scriptedDoer{t: t, responses: [][]byte{resp, nil, nil, nil, nil}}
	cfg := baseConfig(d)
	cfg.ContentLength = 0 // no length signal either

	// The first round yields bytes, so the incompleteness surfaces on read-to-EOF.
	rc, _, err := Open(context.Background(), cfg, nil)
	if err == nil {
		_, err = io.ReadAll(rc)
		rc.Close()
	}
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (status-2 partial, no completion metadata)", err)
	}
	if !strings.Contains(err.Error(), "attestation-pending") {
		t.Errorf("err = %q, want it to name attestation-pending", err)
	}
}

// TestOpenAttestationPendingStillStreams covers STREAM_PROTECTION_STATUS = 2
// (PENDING): non-terminal, the server still streams media, so WaxTap must consume
// it rather than abort. Only status 3 (REQUIRED) is terminal.
func TestOpenAttestationPendingStillStreams(t *testing.T) {
	resp := concat(
		umpFrame(partStreamProtection, marshalStreamProtectionStatus(StreamProtectionStatus{Status: 2})),
		initFrames(0, []byte("INIT")),
		segFrames(1, 1, []byte("AAA")),
		formatInitFrame(1),
	)
	d := &scriptedDoer{t: t, responses: [][]byte{resp}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatalf("status 2 (pending) should stream, got: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAA" {
		t.Errorf("stream = %q, want INITAAA (status-2 media consumed)", got)
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

// TestOpenMissingInitSegmentIsError covers a bare fragment (no init segment, no
// embedded container header): it cannot form a valid file, so it must error
// rather than emit a headerless stream.
func TestOpenMissingInitSegmentIsError(t *testing.T) {
	resp1 := concat(segFrames(1, 1, []byte("AAA")), formatInitFrame(1)) // seq 1, end 1, no init, not self-initializing
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, nil, nil}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (missing init segment)", err)
	}
	if err != nil && !strings.Contains(err.Error(), "init segment") {
		t.Errorf("err = %q, want it to mention the missing init segment", err)
	}
}

// TestOpenSelfInitializingMediaEmitsWithoutInitSegment covers WebM/Opus SABR
// audio, whose first media segment carries the EBML header instead of a separate
// init segment. The stream must emit it rather than stall waiting for an init
// segment that never arrives (the bug behind the false "status 2" report).
func TestOpenSelfInitializingMediaEmitsWithoutInitSegment(t *testing.T) {
	webm := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, []byte("webm-header+cluster")...)
	resp1 := concat(segFrames(1, 1, webm), formatInitFrame(1)) // seq 1, end 1, no separate init
	d := &scriptedDoer{t: t, responses: [][]byte{resp1}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read = %v, want the self-initializing media to stream", err)
	}
	if string(got) != string(webm) {
		t.Errorf("stream = %q, want the self-initializing media verbatim", got)
	}
	if d.calls != 1 {
		t.Errorf("calls = %d, want 1 (the whole stream fits one round)", d.calls)
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

func TestOpenEchoesSabrContextUpdate(t *testing.T) {
	// Mirrors the captured production sequence: part 57 (context update) + an
	// ignored part 67 + part 35 (next-request policy), with no media. The next
	// request must echo the context; then media completes the stream.
	update := SabrContextUpdate{Type: 2, Scope: 1, Value: []byte("CTXBLOB"), SendByDefault: true}
	resp1 := concat(
		umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(update)),
		umpFrame(67, []byte{0x08, 0x01}), // SNACKBAR_MESSAGE-ish; unknown, ignored
		umpFrame(partNextRequestPolicy, marshalNextRequestPolicy(NextRequestPolicy{PlaybackCookie: []byte("c1")})),
	)
	resp2 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(1))
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
		t.Errorf("stream = %q, want INITAAA", got)
	}
	if d.calls != 2 {
		t.Fatalf("calls = %d, want 2 (context round then media round)", d.calls)
	}

	// Round 1 carried no context yet; round 2 echoes it in sabr_contexts (field 5).
	sc1 := protoScan(t, one(t, protoScan(t, d.bodies[0]), 19).b)
	if len(sc1[5]) != 0 {
		t.Errorf("round 1 sabr_contexts = %d, want 0 (nothing to echo yet)", len(sc1[5]))
	}
	sc2 := protoScan(t, one(t, protoScan(t, d.bodies[1]), 19).b)
	assertActiveContext(t, sc2, 2, "CTXBLOB")
}

func TestOpenDuplicateContextStalls(t *testing.T) {
	// The server keeps re-sending the identical context and never any media. The
	// first echo counts as progress, but identical re-sends must not, so the
	// empty-round guard still terminates the stream instead of looping forever
	// (the bug this fix targets: 9+ identical POSTs until timeout).
	update := SabrContextUpdate{Type: 2, Value: []byte("CTXBLOB"), SendByDefault: true}
	ctxResp := umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(update))
	d := &scriptedDoer{t: t, responses: [][]byte{ctxResp, ctxResp, ctxResp}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (stalled, not an infinite loop)", err)
	}
}

func TestOpenKeepExistingContextNotOverwritten(t *testing.T) {
	first := SabrContextUpdate{Type: 2, Value: []byte("V1"), SendByDefault: true}
	// Same type, new value, but KEEP_EXISTING: the stored value must stay V1.
	keep := SabrContextUpdate{Type: 2, Value: []byte("V2"), WritePolicy: writePolicyKeepExisting, SendByDefault: true}
	resp1 := umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(first))
	resp2 := umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(keep))
	resp3 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(1))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, resp2, resp3}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	// Both later requests must echo V1, never the KEEP_EXISTING V2.
	for _, i := range []int{1, 2} {
		sc := protoScan(t, one(t, protoScan(t, d.bodies[i]), 19).b)
		assertActiveContext(t, sc, 2, "V1")
	}
}

func TestOpenContextLifecycle(t *testing.T) {
	// Round 1 stores two contexts: type 2 active (send_by_default), type 3 stored
	// but inactive. A policy then starts 3 and stops 2; a later policy discards 2.
	active := SabrContextUpdate{Type: 2, Value: []byte("V2"), SendByDefault: true}
	stored := SabrContextUpdate{Type: 3, Value: []byte("V3")} // not send_by_default
	resp1 := concat(
		umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(active)),
		umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(stored)),
	)
	resp2 := umpFrame(partSabrContextSendPol, marshalSabrContextSendingPolicy(
		SabrContextSendingPolicy{StartPolicy: []int32{3}, StopPolicy: []int32{2}}))
	resp3 := umpFrame(partSabrContextSendPol, marshalSabrContextSendingPolicy(
		SabrContextSendingPolicy{DiscardPolicy: []int32{2}}))
	resp4 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(1))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, resp2, resp3, resp4}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}

	// Round 2 request (after round 1): type 2 active, type 3 known-but-unsent.
	sc2 := protoScan(t, one(t, protoScan(t, d.bodies[1]), 19).b)
	assertActiveContext(t, sc2, 2, "V2")
	assertUnsent(t, sc2, []uint64{3})

	// Round 3 request (after start 3 / stop 2): type 3 active, type 2 now unsent.
	sc3 := protoScan(t, one(t, protoScan(t, d.bodies[2]), 19).b)
	assertActiveContext(t, sc3, 3, "V3")
	assertUnsent(t, sc3, []uint64{2})

	// Round 4 request (after discard 2): type 2 gone entirely, type 3 still active.
	sc4 := protoScan(t, one(t, protoScan(t, d.bodies[3]), 19).b)
	assertActiveContext(t, sc4, 3, "V3")
	assertUnsent(t, sc4, nil)
}

// assertActiveContext checks that streamerContext sc carries exactly one active
// SABR context (field 5) with the given type and value.
func assertActiveContext(t *testing.T, sc map[protowire.Number][]pfield, typ uint64, value string) {
	t.Helper()
	if len(sc[5]) != 1 {
		t.Fatalf("sabr_contexts(5) = %d, want 1", len(sc[5]))
	}
	ctx := protoScan(t, sc[5][0].b)
	if got := one(t, ctx, 1).v; got != typ {
		t.Errorf("active context type = %d, want %d", got, typ)
	}
	if got := one(t, ctx, 2).b; string(got) != value {
		t.Errorf("active context value = %q, want %q", got, value)
	}
}

// assertUnsent checks the unsent_sabr_contexts (field 6) types, in order. A nil
// want asserts the field is absent.
func assertUnsent(t *testing.T, sc map[protowire.Number][]pfield, want []uint64) {
	t.Helper()
	if len(sc[6]) != len(want) {
		t.Fatalf("unsent_sabr_contexts(6) = %d values, want %d", len(sc[6]), len(want))
	}
	for i, w := range want {
		if got := sc[6][i].v; got != w {
			t.Errorf("unsent[%d] = %d, want %d", i, got, w)
		}
	}
}

func TestOpenContextChurnStalls(t *testing.T) {
	// The value-churn cousin of TestOpenDuplicateContextStalls: a misbehaving
	// server sends a new context value every round, with no media. Each change
	// looks like progress, so the empty-round guard never fires, but
	// maxContextRounds bounds context-only churn, so the stream still stalls
	// instead of looping forever.
	responses := make([][]byte, maxContextRounds)
	for i := range responses {
		u := SabrContextUpdate{Type: 2, Value: []byte{byte('A' + i)}, SendByDefault: true}
		responses[i] = umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(u))
	}
	d := &scriptedDoer{t: t, responses: responses}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (context churn must not loop forever)", err)
	}
	if d.calls != maxContextRounds {
		t.Fatalf("calls = %d, want %d (one POST per context round, then stall)", d.calls, maxContextRounds)
	}
}

func TestOpenStartPolicyForUnknownContextIsNotProgress(t *testing.T) {
	// A SABR_CONTEXT_SENDING_POLICY that starts types the server never delivered
	// has nothing to echo, so it must be ignored: not treated as progress, and
	// not added to the active set (which a server could otherwise grow without
	// bound). With nothing delivered and no media, these are empty rounds, so the
	// stream stalls via the empty-round guard, not the looser context-round cap.
	p := func(typ int32) []byte {
		return umpFrame(partSabrContextSendPol, marshalSabrContextSendingPolicy(
			SabrContextSendingPolicy{StartPolicy: []int32{typ}}))
	}
	d := &scriptedDoer{t: t, responses: [][]byte{p(901), p(902), p(903)}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed", err)
	}
	if d.calls != maxEmptyRounds {
		t.Fatalf("calls = %d, want %d (unknown starts are empty rounds, not context progress)", d.calls, maxEmptyRounds)
	}
}

func TestOpenKeepExistingActivatesStoredContext(t *testing.T) {
	// A context first arrives stored-but-inactive (no send_by_default). A later
	// KEEP_EXISTING update keeps the value but adds send_by_default: the value must
	// stay V, and the type must now be echoed. Activation is independent of the
	// value write policy.
	stored := SabrContextUpdate{Type: 4, Value: []byte("V")} // inactive
	activate := SabrContextUpdate{Type: 4, Value: []byte("IGNORED"), WritePolicy: writePolicyKeepExisting, SendByDefault: true}
	resp1 := umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(stored))
	resp2 := umpFrame(partSabrContextUpdate, marshalSabrContextUpdate(activate))
	resp3 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(1))
	d := &scriptedDoer{t: t, responses: [][]byte{resp1, resp2, resp3}}

	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}

	// Round 2 request (after the stored-inactive round 1): type 4 known but unsent.
	sc2 := protoScan(t, one(t, protoScan(t, d.bodies[1]), 19).b)
	assertUnsent(t, sc2, []uint64{4})
	if len(sc2[5]) != 0 {
		t.Errorf("round 2 sabr_contexts = %d, want 0 (type 4 not yet active)", len(sc2[5]))
	}
	// Round 3 request (after KEEP_EXISTING + send_by_default): now echoed, value kept.
	sc3 := protoScan(t, one(t, protoScan(t, d.bodies[2]), 19).b)
	assertActiveContext(t, sc3, 4, "V")
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
