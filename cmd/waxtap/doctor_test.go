package main

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
)

func TestDoctorIOSBestEffortNote(t *testing.T) {
	cases := []struct {
		name string
		rep  doctorReport
		want bool
	}{
		{"forced ios range check healthy warns", doctorReport{Healthy: true, ForcedIOS: true}, true},
		{"forced ios full run already proved delivery", doctorReport{Healthy: true, Full: true, ForcedIOS: true}, false},
		{"forced ios unhealthy has its own error", doctorReport{Healthy: false, ForcedIOS: true}, false},
		{"not forced ios no note", doctorReport{Healthy: true, ForcedIOS: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := doctorIOSBestEffortNote(&tc.rep)
			if (note != "") != tc.want {
				t.Fatalf("note = %q, want non-empty=%v", note, tc.want)
			}
			if tc.want && !strings.Contains(note, "media delivery is unreliable") {
				t.Errorf("note = %q, want it to warn that iOS media delivery is unreliable", note)
			}
		})
	}
}

// A --full check that delivered a quarter of a megabyte proves the pipeline
// runs, not that a full-length delivery works: the field failures this check
// exists to catch begin just under a megabyte. Saying "healthy" on that sample
// is the wrong answer to the question --full was asked.
func TestDoctorFullSizeNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  doctorReport
		want bool
	}{
		{"full but undersized", doctorReport{Healthy: true, Full: true, Bytes: 246 << 10}, true},
		{"full and large enough", doctorReport{Healthy: true, Full: true, Bytes: 3 << 20}, false},
		{"range check is not full", doctorReport{Healthy: true, Bytes: 64 << 10}, false},
		{"unhealthy says nothing", doctorReport{Full: true, Bytes: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := doctorFullSizeNote(&tc.rep)
			if (note != "") != tc.want {
				t.Fatalf("doctorFullSizeNote = %q, want present=%v", note, tc.want)
			}
			if !tc.want {
				return
			}
			// Both sizes come from humanBytes, so the text cannot drift from the
			// constant it is enforcing.
			for _, want := range []string{humanBytes(tc.rep.Bytes), humanBytes(doctorFullMinBytes)} {
				if !strings.Contains(note, want) {
					t.Errorf("note = %q, want it to quote %q", note, want)
				}
			}
		})
	}

	// The two notes share one slot and must stay mutually exclusive: the iOS note
	// is range-check-only, this one is full-only.
	both := &doctorReport{Healthy: true, Full: true, Bytes: 1, ForcedIOS: true}
	if doctorIOSBestEffortNote(both) != "" {
		t.Error("the iOS note must stay silent on a --full run, or the two notes collide")
	}
}

// --full picks candidates that can actually exercise a full-length delivery.
func TestDoctorFullCandidateOrder(t *testing.T) {
	if len(doctorFullCandidates) == 0 {
		t.Fatal("doctorFullCandidates is empty")
	}
	// The small clip cannot prove full delivery, so it may only be the last
	// resort.
	const small = "jNQXAC9IVRw"
	if doctorFullCandidates[0] == small {
		t.Errorf("--full leads with %s, which is too short to cross %s", small, humanBytes(doctorFullMinBytes))
	}
	if last := doctorFullCandidates[len(doctorFullCandidates)-1]; last != small {
		t.Errorf("last --full candidate = %s, want %s as the connectivity fallback", last, small)
	}
	// Every candidate must be one the plain check already trusts, so --full adds
	// no unverified video IDs.
	for _, id := range doctorFullCandidates {
		if !slices.Contains(doctorCandidates, id) {
			t.Errorf("--full candidate %s is not in doctorCandidates", id)
		}
	}
}

// When the long candidate fails and the short one passes, the run is "healthy"
// but the failure is exactly the finding a full check is looking for. Human
// mode already prints those lines; JSON dropped them.
func TestDoctorJSONAttempts(t *testing.T) {
	decode := func(rep *doctorReport) map[string]any {
		t.Helper()
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
		if err := emitDoctorJSON(env, rep, nil); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		return m
	}

	rep := &doctorReport{Healthy: true, Full: true, VideoID: "jNQXAC9IVRw", Bytes: 1 << 20}
	rep.Attempts = append(rep.Attempts, doctorAttempt{VideoID: "aqz-KE-bpKQ", Error: *errorObject(waxtap.ErrIncompleteStream)})
	m := decode(rep)
	attempts, ok := m["attempts"].([]any)
	if !ok || len(attempts) != 1 {
		t.Fatalf("attempts = %v, want one recorded failure", m["attempts"])
	}
	first, _ := attempts[0].(map[string]any)
	aerr, _ := first["error"].(map[string]any)
	// The same {code, message} object every schemaVersion 3 document reports
	// failures as; a bare string here was the one holdout.
	if first["videoId"] != "aqz-KE-bpKQ" || aerr["code"] != "incomplete-stream" {
		t.Errorf("attempts[0] = %v, want the failed candidate and a coded error object", first)
	}

	if _, ok := decode(&doctorReport{Healthy: true, VideoID: "jNQXAC9IVRw"})["attempts"]; ok {
		t.Error("a clean run must omit attempts, keeping the key additive")
	}
}

// fullDownload describes what the check was asked to do, so a --full run whose
// every candidate failed must not report fullDownload:false as if a range check
// had run.
func TestDoctorFullFlagDescribesMode(t *testing.T) {
	rep := &doctorReport{Full: true} // as the command now sets before checking
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
	if err := emitDoctorJSON(env, rep, errFake("all candidates failed")); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["fullDownload"] != true {
		t.Errorf("fullDownload = %v, want true on a failed --full run", m["fullDownload"])
	}
	// An unhealthy report carries no size note; the note gates on success.
	if doctorFullSizeNote(rep) != "" {
		t.Error("an unhealthy report must not carry the undersized note")
	}
}
