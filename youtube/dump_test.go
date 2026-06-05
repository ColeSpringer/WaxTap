package youtube

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestDumpArtifact_WritesWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dumpEnvVar, dir)

	c := New(Config{})
	c.dumpArtifact(context.Background(), "playerresponse-WEB-abc123.json", []byte(`{"k":1}`))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d files, want 1", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "-playerresponse-WEB-abc123.json") {
		t.Errorf("dump name = %q, want a timestamped *-playerresponse-WEB-abc123.json", name)
	}
	data, err := os.ReadFile(dir + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"k":1}` {
		t.Errorf("dump content = %q", data)
	}
}

func TestDumpArtifact_NoopOnEmptyData(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dumpEnvVar, dir)

	c := New(Config{})
	c.dumpArtifact(context.Background(), "x.json", nil)

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("empty data should write nothing, got %d files", len(entries))
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"playerresponse-WEB-abc.json": "playerresponse-WEB-abc.json",
		"a/b c:d":                     "a_b_c_d",
		"id_-with.dots":               "id_-with.dots",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtract_DumpsOnFailure checks the hook fires during a real extraction
// failure: every client returns a playability error, so each attempt dumps the
// raw response it could not use.
func TestExtract_DumpsOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dumpEnvVar, dir)

	login := readFixture(t, "player_login_required.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			return fixtureResp(http.StatusOK, login), nil
		}
		// Fail the watch-page fallback before it parses, so only player-response
		// dumps are produced.
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	if _, err := c.Extract(context.Background(), "testVideo01"); err == nil {
		t.Fatal("expected extraction to fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one dumped player response on failure")
	}
	for _, e := range entries {
		if !strings.Contains(e.Name(), "playerresponse-") {
			t.Errorf("unexpected dump file: %q", e.Name())
		}
	}
}
