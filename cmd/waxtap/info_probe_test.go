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
}
