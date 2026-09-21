package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
)

func probedInfo() *waxtap.InfoResult {
	return &waxtap.InfoResult{
		Video: &waxtap.Video{
			ID:    "dummyVideo0",
			Title: "T",
			Formats: []waxtap.Format{
				{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", AverageBitrate: 160000, SampleRate: 48000, Channels: 2, Duration: 3 * time.Minute, ContentLength: 4_000_000},
			},
		},
		Client: "WEB_CONTEXT",
		Probed: true,
	}
}

// TestRenderInfoHumanProbedMarker checks the --probe visibility: a (probed) marker
// and the per-format duration appear only when the row was probed.
func TestRenderInfoHumanProbedMarker(t *testing.T) {
	t.Run("probed shows marker and duration", func(t *testing.T) {
		var out bytes.Buffer
		renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, probedInfo(), 0, nil, nil, false)
		got := out.String()
		if !strings.Contains(got, "(probed)") {
			t.Errorf("want the (probed) marker, got:\n%s", got)
		}
		if !strings.Contains(got, "length:  3:00") {
			t.Errorf("want the per-format duration line, got:\n%s", got)
		}
	})

	t.Run("unprobed omits marker and duration", func(t *testing.T) {
		info := probedInfo()
		info.Probed = false
		var out bytes.Buffer
		renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, info, 0, nil, nil, false)
		got := out.String()
		if strings.Contains(got, "(probed)") || strings.Contains(got, "length:") {
			t.Errorf("unprobed info should show neither marker nor duration, got:\n%s", got)
		}
	})
}

// TestEmitInfoJSONOverlaysProbedBest checks that the probed best row's numbers
// land in the deduped formats[] array even when the best row is a later duplicate
// that dedup would otherwise drop.
func TestEmitInfoJSONOverlaysProbedBest(t *testing.T) {
	info := &waxtap.InfoResult{
		Video: &waxtap.Video{
			ID: "dummyVideo0",
			Formats: []waxtap.Format{
				// First occurrence (manifest only); dedup keeps this row.
				{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", AverageBitrate: 160000},
				// bestIdx == 1: same dedup key, but carries the probed numbers.
				{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", AverageBitrate: 160000, SampleRate: 48000, Channels: 2, ContentLength: 4_000_000},
			},
		},
		Probed: true,
	}
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
	if err := emitInfoJSON(env, info, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"sampleRate": 48000`) || !strings.Contains(got, `"channels": 2`) {
		t.Errorf("want the probed best row's numbers in formats[], got:\n%s", got)
	}
	// The document says the numbers are the stream's own, not the listing's.
	if !strings.Contains(got, `"probed": true`) {
		t.Errorf("want probed:true in the document, got:\n%s", got)
	}

	// A run without --probe omits the key rather than asserting false.
	out.Reset()
	info.Probed = false
	if err := emitInfoJSON(env, info, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"probed"`) {
		t.Errorf("want the probed key omitted without --probe, got:\n%s", out.String())
	}
}

// A SABR-only pick cannot be staged, so --probe read nothing and the row still
// carries the manifest's numbers. Saying so is the difference between a probe
// that agreed and a probe that never ran.
func TestInfoNotesProbeSkippedOnSABR(t *testing.T) {
	env := &appEnv{out: io.Discard, errOut: io.Discard, cfg: &appConfig{}, notes: &noteCollector{}}
	info := probedInfo()
	info.Probed = false
	info.BestIndex = 0

	if info.Probed || info.BestIndex < 0 {
		t.Fatal("fixture should describe a resolved row that was not probed")
	}
	noteProbeSkippedIfUnread(env, true, info)
	notes := env.notesJSON()
	if len(notes) != 1 || notes[0].Code != string(noteProbeSkipped) {
		t.Fatalf("notes = %+v, want one %s", notes, noteProbeSkipped)
	}
	if !strings.Contains(notes[0].Detail, "SABR-only") {
		t.Errorf("detail = %q, want it to name the cause", notes[0].Detail)
	}

	// A probe that ran, and a run that did not ask for one, say nothing.
	for _, tc := range []struct {
		name  string
		probe bool
		setup func(*waxtap.InfoResult)
	}{
		{"probed", true, func(i *waxtap.InfoResult) { i.Probed = true }},
		{"not asked", false, func(*waxtap.InfoResult) {}},
		{"nothing resolved", true, func(i *waxtap.InfoResult) { i.BestIndex = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quiet := &appEnv{out: io.Discard, errOut: io.Discard, cfg: &appConfig{}, notes: &noteCollector{}}
			i := probedInfo()
			i.Probed, i.BestIndex = false, 0
			tc.setup(i)
			noteProbeSkippedIfUnread(quiet, tc.probe, i)
			if notes := quiet.notesJSON(); len(notes) != 0 {
				t.Errorf("notes = %+v, want none", notes)
			}
		})
	}
}
