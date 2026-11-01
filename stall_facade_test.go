package waxtap

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hungMediaBody blocks every read until the request ends, as a transport's body
// does once the request behind it is cancelled.
type hungMediaBody struct{ ctx context.Context }

func (b hungMediaBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (hungMediaBody) Close() error { return nil }

// Timeouts.Stall reaches the transfer: a media body that stops arriving is
// abandoned after the window and the stream resumed, instead of hanging until
// the caller gives up.
func TestDownload_StallTimeoutResumesAHungMediaBody(t *testing.T) {
	w := &rotationWorld{media: strings.Repeat("M", 4096), uncapped: true}
	inner := w.roundTrip(t)
	var hung atomic.Bool
	rt := rotationRT(func(r *http.Request) (*http.Response, error) {
		resp, err := inner(r)
		if err == nil && strings.Contains(r.URL.Path, "/videoplayback") && hung.CompareAndSwap(false, true) {
			resp.Body = hungMediaBody{r.Context()} // the headers arrive; the bytes never do
		}
		return resp, err
	})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{
		HTTPClient:       &http.Client{Jar: jar, Transport: rt},
		Client:           "android_vr",
		DisableDiskCache: true,
		Retry:            RetryPolicy{MaxRetries: 1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond},
		Timeouts:         Timeouts{Stall: 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "out.webm")
	done := make(chan error, 1)
	go func() {
		_, err := c.Download(context.Background(), Request{URL: "dummyVideo0", ProcessSpec: ProcessSpec{Output: ToFile(out)}})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stall never fired: Timeouts.Stall did not reach the transfer")
	}
	if !hung.Load() {
		t.Fatal("the test did not exercise the stall")
	}
	if got := len(w.mediaRanges); got != 2 {
		t.Errorf("media requests = %d, want 2 (the hung body and its resume)", got)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != w.media {
		t.Fatalf("output is %d bytes, want the %d-byte media", len(got), len(w.media))
	}
}
