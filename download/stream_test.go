package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
)

func TestToWriter_FullStream(t *testing.T) {
	payload := makePayload(6000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	var buf bytes.Buffer
	res, err := d.ToWriter(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, &buf, nil, nil)
	if err != nil {
		t.Fatalf("ToWriter: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("output mismatch")
	}
}

func TestToWriter_RefreshOnExpiry(t *testing.T) {
	payload := makePayload(6000)
	origin := newTokenOrigin(payload, map[string]bool{"v1": false, "v2": true})
	srv := httptest.NewServer(origin)
	defer srv.Close()

	var refreshes atomic.Int32
	refresh := func(context.Context, *potoken.HTTPFailure) (Source, error) {
		refreshes.Add(1)
		return Source{URL: srv.URL + "?tok=v2", ContentLength: int64(len(payload))}, nil
	}

	d := newTestDownloader(1000, 4)
	var buf bytes.Buffer
	src := Source{URL: srv.URL + "?tok=v1", ContentLength: int64(len(payload))}
	if _, err := d.ToWriter(context.Background(), src, &buf, refresh, nil); err != nil {
		t.Fatalf("ToWriter: %v", err)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh called %d times, want 1", got)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("output mismatch after refresh")
	}
}

// dropOnceOrigin declares the full payload but closes the first response halfway.
// That forces the downloader through its premature-EOF resume path. Later
// requests are served normally.
type dropOnceOrigin struct {
	payload  []byte
	mu       sync.Mutex
	dropped  bool
	resumeAt atomic.Int64
}

func (o *dropOnceOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	first := !o.dropped
	if first {
		o.dropped = true
	}
	o.mu.Unlock()

	if first {
		half := len(o.payload) / 2
		o.resumeAt.Store(int64(half))
		w.Header().Set("Content-Length", strconv.Itoa(len(o.payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(o.payload[:half])
		return // close early: client gets io.ErrUnexpectedEOF and resumes
	}
	serveRange(w, r, o.payload)
}

func TestToWriter_ResumesAfterDrop(t *testing.T) {
	payload := makePayload(6000)
	origin := &dropOnceOrigin{payload: payload}
	srv := httptest.NewServer(origin)
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	var buf bytes.Buffer
	// No refresh needed: a dropped connection is a transient resume, not expiry.
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToWriter(context.Background(), src, &buf, nil, nil); err != nil {
		t.Fatalf("ToWriter: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatalf("output mismatch after resume (got %d bytes, want %d)", buf.Len(), len(payload))
	}
	if origin.resumeAt.Load() == 0 {
		t.Fatal("expected the connection to drop mid-stream")
	}
}

func TestStream_DrainsPayload(t *testing.T) {
	payload := makePayload(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	rc, info, err := d.Stream(context.Background(), src, nil, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	if info.ContentLength != int64(len(payload)) {
		t.Fatalf("StreamInfo.ContentLength = %d, want %d", info.ContentLength, len(payload))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("stream output mismatch")
	}
}

func TestStream_LearnsTotalWhenLengthUnknown(t *testing.T) {
	payload := makePayload(3000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	// ContentLength unknown: the reader should learn it from the first response.
	rc, info, err := d.Stream(context.Background(), Source{URL: srv.URL}, nil, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	if info.ContentLength != int64(len(payload)) {
		t.Fatalf("learned ContentLength = %d, want %d", info.ContentLength, len(payload))
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Fatal("stream output mismatch")
	}
}

func TestStream_EarlyErrorSurfacesFromPrime(t *testing.T) {
	origin := newTokenOrigin(makePayload(2000), map[string]bool{}) // always 403
	srv := httptest.NewServer(origin)
	defer srv.Close()

	refresh := func(context.Context, *potoken.HTTPFailure) (Source, error) {
		return Source{}, fmt.Errorf("provider unavailable: %w", waxerr.ErrNeedsPOToken)
	}

	d := newTestDownloader(1000, 4)
	src := Source{URL: srv.URL + "?tok=x", ContentLength: 2000}
	rc, _, err := d.Stream(context.Background(), src, refresh, nil)
	if !errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Fatalf("err = %v, want ErrNeedsPOToken", err)
	}
	if rc != nil {
		_ = rc.Close()
		t.Fatal("reader should be nil on early error")
	}
}
