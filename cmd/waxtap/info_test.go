package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap"
)

// TestInfoChaptersDetail verifies info --full's chapters are detailed in the
// human list and the JSON chapters array, with an open-ended last chapter that
// omits its end.
func TestInfoChaptersDetail(t *testing.T) {
	noBest := errors.New("no best audio")
	withChapters := &waxtap.InfoResult{
		Video: &waxtap.Video{
			ID: "dummyVideo0", Title: "T", Author: "A",
			Chapters: []waxtap.Chapter{
				{Title: "Intro", Start: 0, End: 30 * time.Second},
				{Title: "Outro", Start: 90 * time.Second}, // open-ended (End == 0)
			},
		},
		Client: "ANDROID_VR",
	}

	t.Run("human list", func(t *testing.T) {
		var out bytes.Buffer
		renderInfoHuman(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}, withChapters, 0, noBest, nil, false)
		got := out.String()
		if !strings.Contains(got, "Chapters:  2") {
			t.Errorf("want the chapter count, got:\n%s", got)
		}
		if !strings.Contains(got, "0:00-0:30  Intro") {
			t.Errorf("want the ranged Intro line, got:\n%s", got)
		}
		// The open-ended last chapter shows only its start.
		if !strings.Contains(got, "1:30  Outro") || strings.Contains(got, "1:30-") {
			t.Errorf("want an open-ended Outro line (start only), got:\n%s", got)
		}
	})

	t.Run("json array", func(t *testing.T) {
		var out bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}, withChapters, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if !strings.Contains(got, `"chapterCount": 2`) {
			t.Errorf("want chapterCount, got:\n%s", got)
		}
		if !strings.Contains(got, `"title": "Intro"`) || !strings.Contains(got, `"endSeconds": 30`) {
			t.Errorf("want the Intro chapter with endSeconds, got:\n%s", got)
		}
		if !strings.Contains(got, `"title": "Outro"`) {
			t.Errorf("want the Outro chapter, got:\n%s", got)
		}
		// Only Intro carries an end; the open-ended Outro omits endSeconds.
		if n := strings.Count(got, "endSeconds"); n != 1 {
			t.Errorf("endSeconds count = %d, want 1 (open-ended chapter omits it):\n%s", n, got)
		}
	})

	t.Run("absent when no chapters", func(t *testing.T) {
		plain := &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "ANDROID_VR"}
		var out bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}, plain, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), `"chapters"`) {
			t.Errorf("JSON should omit the chapters array when empty, got:\n%s", out.String())
		}
	})
}

// TestInfoSubstitutionBreadcrumb verifies that a forced-client fallback to WEB
// is shown in human and JSON output.
func TestInfoSubstitutionBreadcrumb(t *testing.T) {
	noBest := errors.New("no best audio")
	substituted := &waxtap.InfoResult{
		Video:           &waxtap.Video{ID: "dummyVideo0", Title: "T", Author: "A"},
		Client:          "WEB",
		SubstitutedFrom: "WEB_EMBEDDED",
	}

	t.Run("human note", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		renderInfoHuman(env, substituted, 0, noBest, nil, false)
		got := out.String()
		if !strings.Contains(got, "Client:    WEB") {
			t.Errorf("want the Client line, got:\n%s", got)
		}
		if !strings.Contains(got, "requested WEB_EMBEDDED; fell back to WEB") {
			t.Errorf("want the substitution note, got:\n%s", got)
		}
	})

	t.Run("json field", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
		if err := emitInfoJSON(env, substituted, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"substitutedFrom": "WEB_EMBEDDED"`) {
			t.Errorf("want substitutedFrom in JSON, got:\n%s", out.String())
		}
	})

	t.Run("absent when no substitution", func(t *testing.T) {
		plain := &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "ANDROID_VR"}

		var human bytes.Buffer
		renderInfoHuman(&appEnv{out: &human, errOut: io.Discard, cfg: &appConfig{}}, plain, 0, noBest, nil, false)
		if strings.Contains(human.String(), "fell back") {
			t.Errorf("no substitution should print no breadcrumb, got:\n%s", human.String())
		}

		var js bytes.Buffer
		if err := emitInfoJSON(&appEnv{out: &js, errOut: io.Discard, cfg: &appConfig{json: true}}, plain, 0, noBest, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(js.String(), "substitutedFrom") {
			t.Errorf("JSON should omit substitutedFrom when empty, got:\n%s", js.String())
		}
	})
}

// TestWatchPageBreadcrumb covers forced WEB metadata served from the watch-page
// fallback. The note belongs only on a forced WEB read; on the default chain it
// would imply a token issue when the client fallback simply settled on WEB.
func TestWatchPageBreadcrumb(t *testing.T) {
	webViaWatch := &waxtap.InfoResult{
		Video:        &waxtap.Video{ID: "dummyVideo0", Title: "T", Author: "A"},
		Client:       "WEB",
		ViaWatchPage: true,
	}

	t.Run("forced web prints breadcrumb", func(t *testing.T) {
		var errBuf bytes.Buffer
		env := &appEnv{out: io.Discard, errOut: &errBuf, cfg: &appConfig{client: "web"}}
		emitWatchPageBreadcrumb(env, webViaWatch)
		if !strings.Contains(errBuf.String(), "watch-page fallback (no PO token)") {
			t.Errorf("want the watch-page breadcrumb, got:\n%s", errBuf.String())
		}
	})

	t.Run("default chain does not", func(t *testing.T) {
		var errBuf bytes.Buffer
		env := &appEnv{out: io.Discard, errOut: &errBuf, cfg: &appConfig{}}
		emitWatchPageBreadcrumb(env, webViaWatch)
		if errBuf.Len() != 0 {
			t.Errorf("unforced client must print no breadcrumb, got:\n%s", errBuf.String())
		}
	})

	t.Run("forced web but not via watch page", func(t *testing.T) {
		var errBuf bytes.Buffer
		env := &appEnv{out: io.Discard, errOut: &errBuf, cfg: &appConfig{client: "web"}}
		emitWatchPageBreadcrumb(env, &waxtap.InfoResult{Video: &waxtap.Video{ID: "dummyVideo0"}, Client: "WEB"})
		if errBuf.Len() != 0 {
			t.Errorf("a direct WEB read must print no breadcrumb, got:\n%s", errBuf.String())
		}
	})
}
