package youtube

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube/internal/resolver"
)

// fakeResolver records what it was asked to resolve and returns a canned stream.
type fakeResolver struct {
	calls   int
	gotCtx  resolver.Context
	gotCand resolver.Candidate
	stream  resolver.Stream
	err     error
}

func (f *fakeResolver) Resolve(_ context.Context, rc resolver.Context, cand resolver.Candidate) (resolver.Stream, error) {
	f.calls++
	f.gotCtx = rc
	f.gotCand = cand
	return f.stream, f.err
}

// fakeProvider is a stub potoken.Provider.
type fakeProvider struct {
	gotReq potoken.Request
	resp   potoken.Response
	err    error
}

func (f *fakeProvider) ProvidePOToken(_ context.Context, req potoken.Request) (potoken.Response, error) {
	f.gotReq = req
	return f.resp, f.err
}

func newExtraction(profile ClientProfile) *Extraction {
	sess := newSession("US")
	sess.visitorData = "VISITOR1"
	return &Extraction{
		video:   &Video{ID: "vid123"},
		profile: profile,
		session: sess,
		rawAudio: []rawFormat{
			{Itag: 140, SignatureCipher: "s=ABC&url=https%3A%2F%2Fx", ContentLength: "3400000"},
		},
		expiresAt: time.Unix(2000000000, 0).UTC(),
	}
}

func TestResolve_NoTokenNeeded(t *testing.T) {
	fr := &fakeResolver{stream: resolver.Stream{URL: "https://signed/"}}
	c := New(Config{Resolver: fr})

	got, err := c.Resolve(context.Background(), newExtraction(makeProfile(profileAndroidVR)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if fr.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", fr.calls)
	}
	if fr.gotCtx.VideoID != "vid123" {
		t.Errorf("ctx VideoID = %q", fr.gotCtx.VideoID)
	}
	if fr.gotCtx.Token != nil {
		t.Errorf("token = %+v, want nil (ANDROID_VR needs none)", fr.gotCtx.Token)
	}
	if fr.gotCtx.Headers.Get("User-Agent") == "" {
		t.Error("expected a User-Agent header from the winning profile")
	}
	if fr.gotCand.SignatureCipher != "s=ABC&url=https%3A%2F%2Fx" {
		t.Errorf("candidate signatureCipher = %q", fr.gotCand.SignatureCipher)
	}
	// The player response fills content length and expiry the resolver left zero.
	if got.ContentLength != 3400000 {
		t.Errorf("ContentLength = %d, want 3400000 (from raw format)", got.ContentLength)
	}
	if !got.ExpiresAt.Equal(time.Unix(2000000000, 0).UTC()) {
		t.Errorf("ExpiresAt = %v, want extraction fallback", got.ExpiresAt)
	}
}

func TestResolve_TokenRequiredNoProvider(t *testing.T) {
	fr := &fakeResolver{stream: resolver.Stream{URL: "https://signed/"}}
	c := New(Config{Resolver: fr}) // no POTokenProvider

	_, err := c.Resolve(context.Background(), newExtraction(makeProfile(profileWeb)), 0)
	if !errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Fatalf("err = %v, want ErrNeedsPOToken", err)
	}
	if fr.calls != 0 {
		t.Errorf("resolver should not be called when a required token is unavailable (calls=%d)", fr.calls)
	}
}

func TestResolve_TokenFromProvider(t *testing.T) {
	fr := &fakeResolver{stream: resolver.Stream{URL: "https://signed/"}}
	fp := &fakeProvider{resp: potoken.Response{Token: "POT-XYZ"}}
	c := New(Config{Resolver: fr, POTokenProvider: fp})

	if _, err := c.Resolve(context.Background(), newExtraction(makeProfile(profileWeb)), 0); err != nil {
		t.Fatal(err)
	}
	if fp.gotReq.Scope != potoken.ScopeGVS {
		t.Errorf("provider scope = %v, want GVS", fp.gotReq.Scope)
	}
	if fp.gotReq.ClientName != "WEB" || fp.gotReq.VideoID != "vid123" || fp.gotReq.VisitorData != "VISITOR1" {
		t.Errorf("provider request = %+v", fp.gotReq)
	}
	if fr.gotCtx.Token == nil || fr.gotCtx.Token.Value != "POT-XYZ" {
		t.Errorf("token not passed to resolver: %+v", fr.gotCtx.Token)
	}
}

func TestResolve_ProviderReturnsNothing(t *testing.T) {
	fr := &fakeResolver{}
	fp := &fakeProvider{resp: potoken.Response{}} // empty
	c := New(Config{Resolver: fr, POTokenProvider: fp})

	_, err := c.Resolve(context.Background(), newExtraction(makeProfile(profileWeb)), 0)
	if !errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Fatalf("err = %v, want ErrNeedsPOToken", err)
	}
}

func TestResolveWithFailure_ThreadsFailureToProvider(t *testing.T) {
	fr := &fakeResolver{stream: resolver.Stream{URL: "https://signed/"}}
	fp := &fakeProvider{resp: potoken.Response{Token: "POT-XYZ"}}
	c := New(Config{Resolver: fr, POTokenProvider: fp})

	failure := &potoken.HTTPFailure{StatusCode: 403, Status: "403 Forbidden", URL: "https://expired/"}
	if _, err := c.ResolveWithFailure(context.Background(), newExtraction(makeProfile(profileWeb)), 0, failure); err != nil {
		t.Fatal(err)
	}
	if fp.gotReq.Failure == nil || fp.gotReq.Failure.StatusCode != 403 {
		t.Errorf("provider did not receive the triggering failure: %+v", fp.gotReq.Failure)
	}
	// Plain Resolve passes no failure.
	fp.gotReq = potoken.Request{}
	if _, err := c.Resolve(context.Background(), newExtraction(makeProfile(profileWeb)), 0); err != nil {
		t.Fatal(err)
	}
	if fp.gotReq.Failure != nil {
		t.Errorf("Resolve should pass a nil failure, got %+v", fp.gotReq.Failure)
	}
}

func TestResolve_IndexOutOfRange(t *testing.T) {
	c := New(Config{Resolver: &fakeResolver{}})
	_, err := c.Resolve(context.Background(), newExtraction(makeProfile(profileAndroidVR)), 5)
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed", err)
	}
}
