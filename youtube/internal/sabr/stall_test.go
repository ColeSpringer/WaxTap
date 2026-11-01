package sabr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
)

// dripDoer answers every round with body, delivered one byte at a time with gap
// between bytes, so a round takes far longer than the stall window while never
// pausing for it. A cancelled request ends the drip with the context's error,
// as a transport would.
type dripDoer struct {
	body []byte
	gap  time.Duration
}

func (d *dripDoer) Do(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	go func() {
		for _, b := range d.body {
			select {
			case <-req.Context().Done():
				pw.CloseWithError(req.Context().Err())
				return
			case <-time.After(d.gap):
			}
			if _, err := pw.Write([]byte{b}); err != nil {
				return
			}
		}
		pw.Close()
	}()
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: pr, Header: make(http.Header)}, nil
}

// hungBody blocks every read until the request is cancelled.
type hungBody struct{ ctx context.Context }

func (b hungBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (hungBody) Close() error { return nil }

// A round that arrives slowly but steadily finishes: the stall window bounds the
// wait for the next byte, not the round.
func TestOpenSlowRoundFinishes(t *testing.T) {
	resp := concat(
		initFrames(0, []byte("INIT")),
		segFrames(1, 1, []byte("AAA")),
		segFrames(2, 2, []byte("BBB")),
		formatInitFrame(2),
	)
	// Some 60 bytes at 5 ms apart is about 300 ms, five times the window.
	d := &dripDoer{body: resp, gap: 5 * time.Millisecond}
	cfg := baseConfig(d)
	cfg.StallTimeout = 60 * time.Millisecond

	rc, _, err := Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "INITAAABBB" {
		t.Errorf("stream = %q, want INITAAABBB", got)
	}
}

// A round whose body stops arriving fails after the window with the stall
// named, and classifies as a timeout.
func TestOpenHungRoundIsAStall(t *testing.T) {
	d := doerFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: hungBody{req.Context()}, Header: make(http.Header)}, nil
	})
	cfg := baseConfig(d)
	cfg.StallTimeout = 40 * time.Millisecond

	_, _, err := Open(context.Background(), cfg, nil)
	se, ok := errors.AsType[*httpx.StallError](err)
	if !ok {
		t.Fatalf("err = %v, want *httpx.StallError", err)
	}
	if !se.Headers {
		t.Error("Headers = false, want true: the response had begun")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("a stall should classify as a timeout")
	}
}

// A POST the server never answers is abandoned after the window too.
func TestOpenHungResponseIsAStall(t *testing.T) {
	d := doerFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	cfg := baseConfig(d)
	cfg.StallTimeout = 40 * time.Millisecond

	_, _, err := Open(context.Background(), cfg, nil)
	se, ok := errors.AsType[*httpx.StallError](err)
	if !ok {
		t.Fatalf("err = %v, want *httpx.StallError", err)
	}
	if se.Headers {
		t.Error("Headers = true, want false: no response arrived")
	}
}

func TestStallTimeoutDefaultsAndDisables(t *testing.T) {
	s := &stream{}
	if got := s.stallTimeout(); got != httpx.DefaultStallTimeout {
		t.Errorf("zero: stallTimeout() = %v, want the default %v", got, httpx.DefaultStallTimeout)
	}
	s.cfg.StallTimeout = -1
	if got := s.stallTimeout(); got != 0 {
		t.Errorf("negative: stallTimeout() = %v, want 0 (disabled)", got)
	}
	s.cfg.StallTimeout = time.Minute
	if got := s.stallTimeout(); got != time.Minute {
		t.Errorf("stallTimeout() = %v, want 1m", got)
	}
}
