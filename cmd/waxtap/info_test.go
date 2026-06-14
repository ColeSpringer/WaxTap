package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
)

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
