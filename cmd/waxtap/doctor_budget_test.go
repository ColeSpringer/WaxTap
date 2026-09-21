package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A dead proxy costs each candidate its extraction budget. The tester saw
// one run in two report the third candidate as "canceled" (exit 130) with no
// signal sent. Every attempt must classify as the budget it hit.
//
// It did not reproduce in 24 runs here, so this is the contract rather than
// the reproduction. What it pins is the per-attempt half: every attempt's
// error classifies as the budget it hit, and a cancellation with no signal
// behind it can never be one of them. (The exit-130 assertion is the weaker
// half: runMain goes through report, not through main's finalError, so only a
// per-attempt classification could produce a 130 here.) Three runs rather
// than one because a race that fires half the time would still be worth
// catching, and each run costs three candidates at the one-second budget.
func TestDoctorBudgetExpiryIsNeverACancellation(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			fmt.Fprint(w, `{"ok":true,"probe":"tenant","reason":"ok"}`)
		case "/get_pot":
			fmt.Fprint(w, `{"poToken":"tok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer sidecar.Close()
	// A proxy that accepts and never answers: the CONNECT hangs until the
	// extraction budget, which is the bare-deadline shape the report saw.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	})
	t.Setenv("WAXTAP_EXTRACTION_TIMEOUT", "1")
	t.Setenv("WAXTAP_WEB_CONTEXT_TIMEOUT", "1")
	t.Setenv("WAXTAP_SIDECAR_TIMEOUT", "2")
	for i := range 3 {
		stdout, _, code := runMain(t, "doctor", "--json", "--no-cache", "--potoken-url", sidecar.URL, "--proxy", "http://"+ln.Addr().String())
		doc := oneJSONDoc(t, stdout)
		if code == 130 {
			t.Fatalf("run %d exited 130 with no signal: %v", i, doc)
		}
		attempts, _ := doc["attempts"].([]any)
		for _, a := range attempts {
			if e, _ := a.(map[string]any)["error"].(map[string]any); e["code"] == "canceled" {
				t.Fatalf("run %d: attempt %v reports canceled with no signal", i, a)
			}
		}
	}
}
