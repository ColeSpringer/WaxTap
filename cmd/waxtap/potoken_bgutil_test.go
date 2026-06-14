package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/colespringer/waxtap/potoken"
)

func TestBgutilProviderPlayerScope(t *testing.T) {
	var gotBinding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_pot" {
			t.Errorf("path = %q, want /get_pot", r.URL.Path)
		}
		var req bgutilRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotBinding = req.ContentBinding
		_ = json.NewEncoder(w).Encode(bgutilResponse{POToken: "TOKEN-P", ExpiresAt: "2026-06-09T07:25:25Z"})
	}))
	defer srv.Close()

	resp, err := newBgutilProvider(srv.URL+"/get_pot", "").ProvidePOToken(context.Background(), potoken.Request{
		Scope:   potoken.ScopePlayer,
		VideoID: "vid123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBinding != "vid123" {
		t.Errorf("content_binding = %q, want vid123 (player scope binds to the video ID)", gotBinding)
	}
	if resp.Token != "TOKEN-P" {
		t.Errorf("token = %q, want TOKEN-P", resp.Token)
	}
	if want := time.Date(2026, 6, 9, 7, 25, 25, 0, time.UTC); !resp.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (RFC3339)", resp.ExpiresAt, want)
	}
}

// TestBgutilProviderSendsAPIKey verifies the X-API-Key header is sent only when a
// key is configured.
func TestBgutilProviderSendsAPIKey(t *testing.T) {
	for _, key := range []string{"secret-key", ""} {
		var gotKey string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotKey = r.Header.Get("X-API-Key")
			_ = json.NewEncoder(w).Encode(bgutilResponse{POToken: "T"})
		}))
		_, err := newBgutilProvider(srv.URL+"/get_pot", key).ProvidePOToken(context.Background(),
			potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"})
		srv.Close()
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if gotKey != key {
			t.Errorf("X-API-Key = %q, want %q", gotKey, key)
		}
	}
}

func TestBgutilProviderGVSScopeAndEpochExpiry(t *testing.T) {
	var gotBinding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req bgutilRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotBinding = req.ContentBinding
		_ = json.NewEncoder(w).Encode(bgutilResponse{POToken: "TOKEN-G", ExpiresAt: "1812345925"})
	}))
	defer srv.Close()

	// The provider posts to the resolved endpoint without modifying its path.
	resp, err := newBgutilProvider(srv.URL+"/get_pot", "").ProvidePOToken(context.Background(), potoken.Request{
		Scope:       potoken.ScopeGVS,
		VisitorData: "VISITOR==",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBinding != "VISITOR==" {
		t.Errorf("content_binding = %q, want the visitor data (GVS scope)", gotBinding)
	}
	if resp.Token != "TOKEN-G" {
		t.Errorf("token = %q, want TOKEN-G", resp.Token)
	}
	if want := time.Unix(1812345925, 0).UTC(); !resp.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (epoch tolerated)", resp.ExpiresAt, want)
	}
}

func TestBgutilProviderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no integrity token", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := newBgutilProvider(srv.URL+"/get_pot", "").ProvidePOToken(context.Background(),
		potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"}); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}

func TestBgutilProviderBindingErrorsBeforeRequest(t *testing.T) {
	// These must fail in contentBinding, before any HTTP call, so the unroutable
	// address is never contacted.
	p := newBgutilProvider("http://127.0.0.1:0/get_pot", "")
	cases := []potoken.Request{
		{Scope: potoken.ScopePlayer},                  // no video ID
		{Scope: potoken.ScopeGVS},                     // no visitor data
		{Scope: potoken.ScopeSubtitles, VideoID: "v"}, // unsupported scope
	}
	for _, req := range cases {
		if _, err := p.ProvidePOToken(context.Background(), req); err == nil {
			t.Errorf("scope %s: expected an error before any request", req.Scope)
		}
	}
}
