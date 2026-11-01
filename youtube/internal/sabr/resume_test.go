package sabr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// cut describes how a scripted response ends early: after bytes of it, the body
// fails with err, as a dropped connection does, or, with a nil err, stops
// delivering and holds the connection until the request is cancelled, as a
// connection that has stalled does.
type cut struct {
	after int
	err   error
}

// cutDoer answers each round with a scripted body, cutting the rounds named in
// cuts. It records request bodies and the time of each POST. With lengths set
// every response declares its Content-Length, as a server that knows the size
// of its answer does.
type cutDoer struct {
	t         *testing.T
	responses [][]byte
	cuts      map[int]cut   // by 0-based call index
	fails     map[int]error // calls that fail outright, as a request the HTTP client gave up on
	lengths   bool
	calls     int
	bodies    [][]byte
	times     []time.Time
}

func (d *cutDoer) Do(req *http.Request) (*http.Response, error) {
	i := d.calls
	d.calls++
	d.times = append(d.times, time.Now())
	body, _ := io.ReadAll(req.Body)
	d.bodies = append(d.bodies, body)
	if err := d.fails[i]; err != nil {
		return nil, err
	}
	if i >= len(d.responses) {
		d.t.Fatalf("unexpected SABR request #%d to %s", i, req.URL)
	}
	full := d.responses[i]
	resp := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), ContentLength: -1}
	if d.lengths {
		resp.ContentLength = int64(len(full))
	}
	if c, ok := d.cuts[i]; ok {
		resp.Body = &cutBody{data: full[:c.after], err: c.err, ctx: req.Context()}
	} else {
		resp.Body = io.NopCloser(bytes.NewReader(full))
	}
	return resp, nil
}

// cutBody delivers data, then fails with err, or with a nil err blocks until
// the request is cancelled and reports the cancellation, as a transport does.
type cutBody struct {
	data []byte
	err  error
	ctx  context.Context
}

func (b *cutBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*cutBody) Close() error { return nil }

// readStream drains an opened stream.
func readStream(t *testing.T, cfg Config) ([]byte, error) {
	t.Helper()
	rc, _, err := Open(context.Background(), cfg, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// A round that ends part way, whether its body stalled or its connection
// dropped, is not the end of the stream: the segments that arrived whole are
// kept and acknowledged, the segment the cut fell inside is dropped, and the
// next round asks the server to go on from there, as the reference client does.
func TestOpenCutRoundResumesWithWholeSegments(t *testing.T) {
	for name, c := range map[string]cut{
		"stalled": {err: nil},
		"dropped": {err: io.ErrUnexpectedEOF},
	} {
		t.Run(name, func(t *testing.T) {
			arrived := concat(
				initFrames(0, []byte("INIT")),
				segFrames(1, 1, []byte("AAA")),
				segFramesOpen(2, 2, []byte("BBB")), // its MEDIA_END never arrives
			)
			c.after = len(arrived)
			resp1 := concat(arrived, mediaEndFrame(2), formatInitFrame(3),
				umpFrame(partNextRequestPolicy, marshalNextRequestPolicy(NextRequestPolicy{PlaybackCookie: []byte("c1")})))
			resp2 := concat(segFrames(5, 2, []byte("BBB")), segFrames(6, 3, []byte("CCC")), formatInitFrame(3))
			d := &cutDoer{t: t, responses: [][]byte{resp1, resp2}, cuts: map[int]cut{0: c}}
			cfg := baseConfig(d)
			cfg.StallTimeout = 40 * time.Millisecond

			got, err := readStream(t, cfg)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != "INITAAABBBCCC" {
				t.Errorf("stream = %q, want INITAAABBBCCC", got)
			}
			if d.calls != 2 {
				t.Fatalf("calls = %d, want 2 (the cut round and the one that resumed it)", d.calls)
			}
			// The resume acknowledges segment 1 alone: segment 2 was cut and must be
			// sent again. The policy was lost with the tail, so no cookie is echoed.
			req2 := protoScan(t, d.bodies[1])
			if len(req2[3]) != 1 {
				t.Fatalf("request2 buffered_ranges = %d, want 1", len(req2[3]))
			}
			br := protoScan(t, req2[3][0].b)
			if start, end := one(t, br, 4).v, one(t, br, 5).v; start != 1 || end != 1 {
				t.Errorf("request2 acked [%d,%d], want [1,1]: the cut segment is not buffered", start, end)
			}
			sc := protoScan(t, one(t, req2, 19).b)
			if len(sc[3]) != 0 {
				t.Errorf("request2 echoed a playback cookie the cut round never delivered")
			}
		})
	}
}

// A segment whose bytes reach the header's content length is whole even when the
// cut took its MEDIA_END, so it is kept and acknowledged.
func TestOpenCutRoundKeepsSegmentWithMatchingLength(t *testing.T) {
	sized := concat(
		umpFrame(partMediaHeader, marshalMediaHeader(MediaHeader{HeaderID: 2, Itag: 251, SequenceNumber: 2, DurationMs: 5000, ContentLength: 3})),
		mediaFrame(2, []byte("BBB")),
	)
	arrived := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), sized)
	resp1 := concat(arrived, mediaEndFrame(2), formatInitFrame(3))
	resp2 := concat(segFrames(6, 3, []byte("CCC")), formatInitFrame(3))
	d := &cutDoer{t: t, responses: [][]byte{resp1, resp2}, cuts: map[int]cut{0: {after: len(arrived), err: io.ErrUnexpectedEOF}}}

	got, err := readStream(t, baseConfig(d))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBBCCC" {
		t.Errorf("stream = %q, want INITAAABBBCCC", got)
	}
	req2 := protoScan(t, d.bodies[1])
	if len(req2[3]) != 1 {
		t.Fatalf("request2 buffered_ranges = %d, want 1", len(req2[3]))
	}
	br := protoScan(t, req2[3][0].b)
	if start, end := one(t, br, 4).v, one(t, br, 5).v; start != 1 || end != 2 {
		t.Errorf("request2 acked [%d,%d], want [1,2]: the sized segment was whole", start, end)
	}
}

// Rounds that keep ending part way without adding anything spend the budget,
// and once the stream has delivered bytes a spent budget is an incomplete
// delivery, with the last cut kept as the cause, so another client may finish
// the stream.
func TestOpenCutRoundsAfterDeliveryAreIncomplete(t *testing.T) {
	resp1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(3))
	hung := cut{after: 0} // answers, then nothing
	d := &cutDoer{t: t, responses: [][]byte{resp1, nil, nil, nil}, cuts: map[int]cut{1: hung, 2: hung, 3: hung}}
	cfg := baseConfig(d)
	cfg.StallTimeout = 40 * time.Millisecond
	cfg.MaxRoundRetries = 2

	got, err := readStream(t, cfg)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream: bytes had been delivered", err)
	}
	if se, ok := errors.AsType[*httpx.StallError](err); !ok || !se.Headers {
		t.Errorf("err = %v, want the body stall kept as the cause", err)
	}
	if !strings.Contains(err.Error(), "after 7 bytes") {
		t.Errorf("err = %q, want the delivered byte count named", err)
	}
	if string(got) != "INITAAA" {
		t.Errorf("delivered prefix = %q, want INITAAA", got)
	}
	if d.calls != 4 {
		t.Errorf("calls = %d, want 4: the round that delivered, then one attempt plus two retries", d.calls)
	}
}

// A cut round that added media refunds the budget: a connection that drops
// every round but keeps delivering is a stream that is moving, not failing.
func TestOpenCutRoundsWithProgressRefundBudget(t *testing.T) {
	round := func(seq uint64, whole, open []byte) ([]byte, int) {
		arrived := concat(segFrames(uint32(seq), seq, whole), segFramesOpen(uint32(seq+1), seq+1, open))
		return arrived, len(arrived)
	}
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), segFramesOpen(2, 2, []byte("BBB")))
	r2, cut2 := round(2, []byte("BBB"), []byte("CCC"))
	r3, cut3 := round(3, []byte("CCC"), []byte("DDD"))
	r4 := concat(segFrames(4, 4, []byte("DDD")), formatInitFrame(4))
	d := &cutDoer{t: t, responses: [][]byte{r1, r2, r3, r4}, cuts: map[int]cut{
		0: {after: len(r1), err: io.ErrUnexpectedEOF},
		1: {after: cut2, err: io.ErrUnexpectedEOF},
		2: {after: cut3, err: io.ErrUnexpectedEOF},
	}}
	cfg := baseConfig(d)
	cfg.MaxRoundRetries = 1 // two consecutive cuts without progress would end it

	got, err := readStream(t, cfg)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBBCCCDDD" {
		t.Errorf("stream = %q, want INITAAABBBCCCDDD", got)
	}
	if d.calls != 4 {
		t.Errorf("calls = %d, want 4", d.calls)
	}
}

// A cut round is not followed at once: one that added media waits a base step,
// so a connection that drops every round is not asked again back to back, and
// one that added nothing waits its attempt's backoff.
func TestOpenCutRoundWaitsBeforeResuming(t *testing.T) {
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(2))
	r3 := segFrames(2, 2, []byte("BBB"))
	d := &cutDoer{t: t, responses: [][]byte{r1, nil, r3}, cuts: map[int]cut{
		0: {after: len(r1), err: io.ErrUnexpectedEOF}, // progress, then the drop
		1: {after: 0},                                 // answers, then stalls
	}}
	cfg := baseConfig(d)
	cfg.StallTimeout = 40 * time.Millisecond
	cfg.RetryBackoff = 40 * time.Millisecond

	got, err := readStream(t, cfg)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB", got)
	}
	if d.calls != 3 {
		t.Fatalf("calls = %d, want 3", d.calls)
	}
	if gap := d.times[1].Sub(d.times[0]); gap < 30*time.Millisecond {
		t.Errorf("resume after progress came %v after the cut round, want the 40ms base step", gap)
	}
	if gap := d.times[2].Sub(d.times[1]); gap < 70*time.Millisecond {
		t.Errorf("retry came %v after the hung round, want the 40ms stall plus the 40ms backoff", gap)
	}
}

// A body whose final bytes arrive with an error, as an HTTP/2 abort can add, is
// whole when they reach the declared Content-Length: nothing was cut, so the
// stream goes straight on without a resume.
func TestOpenTrailingErrorOnWholeBodyIsNotACut(t *testing.T) {
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(2))
	r2 := segFrames(2, 2, []byte("BBB"))
	d := &cutDoer{t: t, responses: [][]byte{r1, r2}, lengths: true, cuts: map[int]cut{0: {after: len(r1), err: io.ErrUnexpectedEOF}}}
	var logs bytes.Buffer
	cfg := baseConfig(d)
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	got, err := readStream(t, cfg)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB", got)
	}
	if d.calls != 2 {
		t.Errorf("calls = %d, want 2", d.calls)
	}
	if strings.Contains(logs.String(), "round cut short") {
		t.Errorf("a whole body was treated as a cut round:\n%s", logs.String())
	}
}

// A segment the server never closed is not whole even in a body that ended
// cleanly: a close-delimited response or a proxy can end a body at a part
// boundary part way through a segment with no error at all. Unclosed and
// unsized, it is dropped unacknowledged and the server sends it again.
func TestOpenUnclosedSegmentInFinishedRoundIsDropped(t *testing.T) {
	resp1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(3),
		segFramesOpen(2, 2, []byte("BB"))) // ends at a part boundary, two of segment 2's three bytes
	resp2 := concat(segFrames(5, 2, []byte("BBB")), segFrames(6, 3, []byte("CCC")))
	d := &cutDoer{t: t, responses: [][]byte{resp1, resp2}}

	got, err := readStream(t, baseConfig(d))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBBCCC" {
		t.Errorf("stream = %q, want INITAAABBBCCC (the unclosed segment replaced, never written)", got)
	}
	req2 := protoScan(t, d.bodies[1])
	if len(req2[3]) != 1 {
		t.Fatalf("request2 buffered_ranges = %d, want 1", len(req2[3]))
	}
	br := protoScan(t, req2[3][0].b)
	if start, end := one(t, br, 4).v, one(t, br, 5).v; start != 1 || end != 1 {
		t.Errorf("request2 acked [%d,%d], want [1,1]: an unclosed segment is not buffered", start, end)
	}
}

// The caller giving up is not a cut: a body read that fails because the stream
// was closed or its context cancelled reports the cancellation, with nothing
// salvaged, retried, or recast as an incomplete delivery.
func TestOpenCancellationDuringBodyIsNotACut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(3))
	var calls int
	d := doerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		resp := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), ContentLength: -1}
		if calls == 1 {
			resp.Body = io.NopCloser(bytes.NewReader(r1))
			return resp, nil
		}
		// The second round's body never arrives, and the caller gives up on it.
		time.AfterFunc(10*time.Millisecond, cancel)
		resp.Body = &cutBody{ctx: req.Context()}
		return resp, nil
	})
	var logs bytes.Buffer
	cfg := baseConfig(d)
	cfg.MaxRoundRetries = -1 // no budget left to hide behind
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	rc, _, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Error("a cancellation must not be reported as an incomplete delivery")
	}
	if string(got) != "INITAAA" || calls != 2 {
		t.Errorf("delivered %q over %d calls, want INITAAA over 2", got, calls)
	}
	if strings.Contains(logs.String(), "round cut short") {
		t.Errorf("a cancellation was treated as a cut round:\n%s", logs.String())
	}
}

// A cut round that added media is media progress for the stall guards too: an
// empty round before it and one after it are not consecutive.
func TestOpenCutRoundWithMediaClearsEmptyRounds(t *testing.T) {
	r2 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), segFramesOpen(2, 2, []byte("BBB")))
	r4 := concat(segFrames(5, 2, []byte("BBB")), formatInitFrame(2))
	d := &cutDoer{t: t, responses: [][]byte{nil, r2, nil, r4}, cuts: map[int]cut{1: {after: len(r2), err: io.ErrUnexpectedEOF}}}

	got, err := readStream(t, baseConfig(d))
	if err != nil {
		t.Fatalf("read: %v (the empty rounds around a productive cut round must not count as consecutive)", err)
	}
	if string(got) != "INITAAABBB" || d.calls != 4 {
		t.Errorf("delivered %q over %d calls, want INITAAABBB over 4", got, d.calls)
	}
}

// A MEDIA_END whose payload cannot be read is ignored, like any other part this
// client does not understand; it does not end the stream.
func TestOpenUndecodableMediaEndIsIgnored(t *testing.T) {
	sized := concat(
		umpFrame(partMediaHeader, marshalMediaHeader(MediaHeader{HeaderID: 1, Itag: 251, SequenceNumber: 1, DurationMs: 5000, ContentLength: 3})),
		mediaFrame(1, []byte("AAA")),
		umpFrame(partMediaEnd, nil), // no header id
	)
	resp1 := concat(initFrames(0, []byte("INIT")), sized, formatInitFrame(1))
	d := &cutDoer{t: t, responses: [][]byte{resp1}}

	got, err := readStream(t, baseConfig(d))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAA" || d.calls != 1 {
		t.Errorf("delivered %q over %d calls, want INITAAA over 1", got, d.calls)
	}
}

// A request the HTTP client gave up on, after the stream has delivered bytes,
// is an incomplete delivery like a spent cut budget: the stream ended before
// completion and another client may finish it. Before any bytes it is the
// failure itself.
func TestOpenRequestFailureAfterDeliveryIsIncomplete(t *testing.T) {
	refused := errors.New("connection refused")
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(3))
	d := &cutDoer{t: t, responses: [][]byte{r1, nil}, fails: map[int]error{1: refused}}

	got, err := readStream(t, baseConfig(d))
	if !errors.Is(err, waxerr.ErrIncompleteStream) || !errors.Is(err, refused) {
		t.Fatalf("err = %v, want ErrIncompleteStream with the failure as its cause", err)
	}
	if !strings.Contains(err.Error(), "after 7 bytes") {
		t.Errorf("err = %q, want the delivered byte count named", err)
	}
	if string(got) != "INITAAA" {
		t.Errorf("delivered prefix = %q, want INITAAA", got)
	}

	d = &cutDoer{t: t, responses: [][]byte{nil}, fails: map[int]error{0: refused}}
	if _, _, err := Open(context.Background(), baseConfig(d), nil); !errors.Is(err, refused) || errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Errorf("err = %v, want the failure itself: nothing had been delivered", err)
	}
}

// A header id names one segment at a time. Once the server has closed a segment
// with MEDIA_END it may use the id again, and the next header under it starts a
// new segment rather than extending the closed one.
func TestOpenReusedHeaderIDStartsANewSegment(t *testing.T) {
	// Both segments arrive under header id 1 in one finished round.
	resp1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), segFrames(1, 2, []byte("BBB")), formatInitFrame(3))
	resp2 := segFrames(1, 3, []byte("CCC"))
	d := &cutDoer{t: t, responses: [][]byte{resp1, resp2}}

	got, err := readStream(t, baseConfig(d))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBBCCC" {
		t.Errorf("stream = %q, want INITAAABBBCCC", got)
	}
	req2 := protoScan(t, d.bodies[1])
	if len(req2[3]) != 1 {
		t.Fatalf("request2 buffered_ranges = %d, want 1", len(req2[3]))
	}
	br := protoScan(t, req2[3][0].b)
	if start, end := one(t, br, 4).v, one(t, br, 5).v; start != 1 || end != 2 {
		t.Errorf("request2 acked [%d,%d], want [1,2]: two segments, not one merged", start, end)
	}
	cas := protoScan(t, one(t, req2, 1).b)
	if got := one(t, cas, 28).v; got != 10000 {
		t.Errorf("request2 player_time_ms = %d, want 10000 (two 5 s segments)", got)
	}

	// The reused id's second segment is the one the cut falls inside: closing
	// the first did not close it.
	arrived := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), segFramesOpen(1, 2, []byte("BB")))
	resp1 = concat(arrived, mediaEndFrame(1), formatInitFrame(2))
	resp2 = concat(segFrames(5, 2, []byte("BBB")), formatInitFrame(2))
	d = &cutDoer{t: t, responses: [][]byte{resp1, resp2}, cuts: map[int]cut{0: {after: len(arrived), err: io.ErrUnexpectedEOF}}}
	got, err = readStream(t, baseConfig(d))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB (the cut segment under a reused id is not whole)", got)
	}
}

// The wait after a cut round, like a server's backoff, is taken before the next
// POST rather than inside the round that earned it, so the bytes the round did
// deliver reach the reader at once instead of after the wait.
func TestOpenSalvagedBytesAreReadableBeforeTheWait(t *testing.T) {
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), segFramesOpen(2, 2, []byte("BBB")))
	d := &cutDoer{t: t, responses: [][]byte{r1, nil}, cuts: map[int]cut{0: {after: len(r1), err: io.ErrUnexpectedEOF}}}
	cfg := baseConfig(d)
	cfg.RetryBackoff = 2 * time.Second

	start := time.Now()
	rc, _, err := Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	buf := make([]byte, 16)
	n, err := rc.Read(buf)
	if err != nil || string(buf[:n]) != "INITAAA" {
		t.Fatalf("first Read = (%q, %v), want INITAAA", buf[:n], err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the first bytes took %v to reach the reader; the 2s wait must fall before the next POST, not before these bytes", waited)
	}
	if d.calls != 1 {
		t.Errorf("calls = %d, want 1: the next POST waits for the backoff", d.calls)
	}
}

// A server backoff is honored the same way: the round's bytes are readable
// while the wait runs ahead of the next POST.
func TestOpenServerBackoffDoesNotHoldDeliveredBytes(t *testing.T) {
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(2), policyFrame(2000))
	d := &cutDoer{t: t, responses: [][]byte{r1, nil}}

	start := time.Now()
	rc, _, err := Open(context.Background(), baseConfig(d), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	buf := make([]byte, 16)
	n, err := rc.Read(buf)
	if err != nil || string(buf[:n]) != "INITAAA" {
		t.Fatalf("first Read = (%q, %v), want INITAAA", buf[:n], err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the first bytes took %v to reach the reader; the server's 2s backoff must not hold them", waited)
	}
	if d.calls != 1 {
		t.Errorf("calls = %d, want 1", d.calls)
	}
}

// A segment whose bytes do not match the content length its header declares is
// not written, even in a round the server finished: it is dropped unacknowledged
// so the server sends it again, as the reference client does.
func TestOpenSegmentLengthMismatchIsDropped(t *testing.T) {
	short := concat(
		umpFrame(partMediaHeader, marshalMediaHeader(MediaHeader{HeaderID: 1, Itag: 251, SequenceNumber: 1, DurationMs: 5000, ContentLength: 10})),
		mediaFrame(1, []byte("AAA")), // 3 of the declared 10 bytes
		mediaEndFrame(1),
	)
	resp1 := concat(initFrames(0, []byte("INIT")), short, formatInitFrame(2))
	resp2 := concat(segFrames(5, 1, []byte("AAA")), segFrames(6, 2, []byte("BBB")))
	d := &cutDoer{t: t, responses: [][]byte{resp1, resp2}}

	got, err := readStream(t, baseConfig(d))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB (the short segment replaced, not written)", got)
	}
	if d.calls != 2 {
		t.Fatalf("calls = %d, want 2", d.calls)
	}
	if req2 := protoScan(t, d.bodies[1]); len(req2[3]) != 0 {
		t.Errorf("request2 buffered_ranges = %d, want 0: a dropped segment is not acknowledged", len(req2[3]))
	}
}

// A signal that arrived whole before the cut stands: the server's verdict does
// not need the rest of the body.
func TestOpenCutRoundSalvagesSignal(t *testing.T) {
	arrived := concat(
		initFrames(0, []byte("INIT")),
		umpFrame(partStreamProtection, marshalStreamProtectionStatus(StreamProtectionStatus{Status: 3})),
	)
	resp1 := concat(arrived, segFrames(1, 1, []byte("AAA")))
	d := &cutDoer{t: t, responses: [][]byte{resp1}, cuts: map[int]cut{0: {after: len(arrived), err: io.ErrUnexpectedEOF}}}

	_, _, err := Open(context.Background(), baseConfig(d), nil)
	if !errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Fatalf("err = %v, want ErrNeedsPOToken from the salvaged status", err)
	}
}

func TestRoundRetryDefaultsAndDisables(t *testing.T) {
	s := &stream{}
	if got := s.maxRoundRetries(); got != defaultMaxRoundRetries {
		t.Errorf("zero: maxRoundRetries() = %d, want the default %d", got, defaultMaxRoundRetries)
	}
	s.cfg.MaxRoundRetries = -1
	if got := s.maxRoundRetries(); got != 0 {
		t.Errorf("negative: maxRoundRetries() = %d, want 0 (disabled)", got)
	}
	s.cfg.MaxRoundRetries = 5
	if got := s.maxRoundRetries(); got != 5 {
		t.Errorf("maxRoundRetries() = %d, want 5", got)
	}
	if got := s.retryBackoff(0); got != defaultRetryBackoff {
		t.Errorf("retryBackoff(0) = %v, want the default base %v", got, defaultRetryBackoff)
	}
	if got := s.retryBackoff(1); got != 2*defaultRetryBackoff {
		t.Errorf("retryBackoff(1) = %v, want twice the base", got)
	}
	if got := s.retryBackoff(40); got != defaultRetryMaxBackoff {
		t.Errorf("retryBackoff(40) = %v, want the cap %v", got, defaultRetryMaxBackoff)
	}
	s.cfg.RetryBackoff, s.cfg.RetryMaxBackoff = time.Second, 3*time.Second
	if got := s.retryBackoff(5); got != 3*time.Second {
		t.Errorf("retryBackoff(5) = %v, want the configured 3s cap", got)
	}
}

// With the retries disabled a cut round ends the stream as it always did: as
// the cut itself before anything was delivered, as an incomplete delivery after.
func TestOpenCutRoundWithRetriesDisabledEndsStream(t *testing.T) {
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(2))
	d := &cutDoer{t: t, responses: [][]byte{r1, nil}, cuts: map[int]cut{1: {after: 0, err: io.ErrUnexpectedEOF}}}
	cfg := baseConfig(d)
	cfg.MaxRoundRetries = -1

	got, err := readStream(t, cfg)
	if !errors.Is(err, waxerr.ErrIncompleteStream) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrIncompleteStream with the cut as its cause", err)
	}
	if string(got) != "INITAAA" || d.calls != 2 {
		t.Errorf("delivered %q over %d calls, want INITAAA over 2: no retry", got, d.calls)
	}

	d = &cutDoer{t: t, responses: [][]byte{nil}, cuts: map[int]cut{0: {after: 0, err: io.ErrUnexpectedEOF}}}
	cfg = baseConfig(d)
	cfg.MaxRoundRetries = -1
	if _, _, err := Open(context.Background(), cfg, nil); !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Errorf("err = %v, want the cut itself: nothing had been delivered", err)
	}
}

// A re-sent init segment, which the server sends after a round it did not
// finish, is the one already written and is passed over quietly; a different
// init after the output began is still reported.
func TestOpenResentInitSegmentIsIgnoredQuietly(t *testing.T) {
	var logs bytes.Buffer
	r1 := concat(initFrames(0, []byte("INIT")), segFrames(1, 1, []byte("AAA")), formatInitFrame(3))
	r2 := concat(initFrames(0, []byte("INIT")), segFrames(1, 2, []byte("BBB")))
	r3 := concat(initFrames(0, []byte("OTHER")), segFrames(1, 3, []byte("CCC")))
	d := &cutDoer{t: t, responses: [][]byte{r1, r2, r3}}
	cfg := baseConfig(d)
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got, err := readStream(t, cfg)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "INITAAABBBCCC" {
		t.Errorf("stream = %q, want INITAAABBBCCC", got)
	}
	if n := strings.Count(logs.String(), "init segment arrived after media"); n != 1 {
		t.Errorf("warnings about a late init = %d, want 1: only the different init is reported\n%s", n, logs.String())
	}
}
