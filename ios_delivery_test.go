package waxtap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// iosRoundTrip records whether iOS extraction or delivery was attempted.
type iosRoundTrip struct {
	status    int
	playerHit bool
}

func (rt *iosRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/player") || strings.Contains(r.URL.Path, "/videoplayback") {
		rt.playerHit = true
	}
	return &http.Response{
		StatusCode: rt.status,
		Status:     http.StatusText(rt.status),
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

func TestForcedIOSAttemptsDelivery(t *testing.T) {
	rt := &iosRoundTrip{status: http.StatusNotFound}
	c, err := New(Options{Client: "ios", HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}

	_, derr := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		ProcessSpec: ProcessSpec{Output: ToFile(t.TempDir() + "/out.webm")},
	})
	if derr == nil {
		t.Fatal("want an error from the failed iOS extraction, got nil")
	}
	if errors.Is(derr, ErrDeliveryUnsupported) {
		t.Errorf("forced iOS returned ErrDeliveryUnsupported: %v", derr)
	}
	if !rt.playerHit {
		t.Error("the forced iOS chain should reach the /player path and attempt delivery")
	}
}

func TestForcedIOSSkipExisting(t *testing.T) {
	c, err := New(Options{Client: "ios"})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir() + "/out.webm"
	if f, werr := os.Create(out); werr != nil {
		t.Fatal(werr)
	} else {
		f.Close()
	}
	res, derr := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		ProcessSpec: ProcessSpec{Output: ToFile(out), SkipIfExists: true},
	})
	if derr != nil {
		t.Fatalf("SkipIfExists with an existing file should skip, not fail: %v", derr)
	}
	if res == nil || res.OutputPath != out {
		t.Errorf("res = %+v, want a skipped Result for the existing output", res)
	}
}
