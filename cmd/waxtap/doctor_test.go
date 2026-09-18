package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/potoken"
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

type fakeSessionProvider struct {
	sess potoken.Session
	err  error
}

func (f fakeSessionProvider) ProvideSession(context.Context) (potoken.Session, error) {
	return f.sess, f.err
}

type fakeContextProvider struct{ err error }

func (f fakeContextProvider) ProvidePlayerContext(context.Context, string) (potoken.PlayerContext, error) {
	return potoken.PlayerContext{}, f.err
}

type fakeTokenProvider struct {
	tok string
	err error
	req potoken.Request // the last request, so the probe's scope and binding can be checked
}

func (f *fakeTokenProvider) ProvidePOToken(_ context.Context, req potoken.Request) (potoken.Response, error) {
	f.req = req
	return potoken.Response{Token: f.tok}, f.err
}

func TestProbeSidecars(t *testing.T) {
	refusal := &waxtap.SidecarResponseError{Label: "player-context server", Endpoint: "http://127.0.0.1:4416/player-context", StatusCode: 502, Code: "player-context-failed", RetryAfter: 25 * time.Second, Reason: "proof cool-down"}
	tokenFake := &fakeTokenProvider{tok: "tok"}
	probes := probeSidecars(context.Background(), sidecarProviders{
		session: fakeSessionProvider{sess: potoken.Session{VisitorData: "vd"}},
		context: fakeContextProvider{err: refusal},
		token:   tokenFake,
	}, "dummyVideo0", time.Minute)
	if len(probes) != 3 || probes[0].Endpoint != "session" || !probes[0].OK || probes[1].Endpoint != "po-token" || !probes[1].OK {
		t.Fatalf("probes = %+v", probes)
	}
	pc := probes[2]
	if pc.Endpoint != "player-context" || pc.OK || pc.StatusCode != 502 || pc.Code != "player-context-failed" || pc.RetryAfterSeconds != 25 || pc.Error == nil || pc.Error.Code != "network" {
		t.Errorf("player-context probe = %+v", pc)
	}
	// The token probe minted the GVS token the download needs, bound to the
	// session probe's visitorData.
	if got := tokenFake.req; got.Scope != potoken.ScopeGVS || got.VisitorData != "vd" {
		t.Errorf("token probe request = %+v, want GVS scope bound to the session's visitorData", got)
	}
	if !sidecarProbesHealthy(probes[:2]) || sidecarProbesHealthy(probes) {
		t.Error("a failed probe must make the report unhealthy")
	}
	if err := firstSidecarProbeError(probes); err != refusal {
		t.Errorf("firstSidecarProbeError = %v, want the refusal itself", err)
	}

	// Without a session sidecar the token probe falls back to a player-scope
	// request on the video ID: there is no visitorData to bind to.
	lone := &fakeTokenProvider{tok: "tok"}
	probes = probeSidecars(context.Background(), sidecarProviders{token: lone}, "dummyVideo0", time.Minute)
	if len(probes) != 1 || probes[0].Endpoint != "po-token" {
		t.Fatalf("probes = %+v, want only the configured one", probes)
	}
	if lone.req.Scope != potoken.ScopePlayer || lone.req.VideoID != "dummyVideo0" {
		t.Errorf("token probe request = %+v, want a player-scope request on the video", lone.req)
	}

	if got := probeSidecars(context.Background(), sidecarProviders{}, "dummyVideo0", time.Minute); len(got) != 0 {
		t.Errorf("no sidecars configured, got %+v", got)
	}
}

func TestDoctorJSONSidecars(t *testing.T) {
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

	refusal := &waxtap.SidecarResponseError{Label: "player-context server", Endpoint: "http://127.0.0.1:4416/player-context", StatusCode: 502, Code: "player-context-failed", RetryAfter: 25 * time.Second}
	probes := probeSidecars(context.Background(), sidecarProviders{
		session: fakeSessionProvider{sess: potoken.Session{VisitorData: "vd"}},
		context: fakeContextProvider{err: refusal},
	}, "dummyVideo0", time.Minute)
	rep := &doctorReport{Delivered: true, Healthy: sidecarProbesHealthy(probes), VideoID: "jNQXAC9IVRw", Sidecars: probes}
	m := decode(rep)
	if rep.Healthy {
		t.Error("a failed probe must make the report unhealthy")
	}
	arr, ok := m["sidecars"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("sidecars = %v, want two probes", m["sidecars"])
	}
	second, _ := arr[1].(map[string]any)
	if second["endpoint"] != "player-context" || second["code"] != "player-context-failed" || second["retryAfterSeconds"] != float64(25) {
		t.Errorf("sidecars[1] = %v, want the code and the stated wait", second)
	}

	if _, ok := decode(&doctorReport{Healthy: true, VideoID: "jNQXAC9IVRw"})["sidecars"]; ok {
		t.Error("a report without probes must omit sidecars, keeping the key additive")
	}
}

func TestDoctorHumanSidecarLines(t *testing.T) {
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
	rep := &doctorReport{
		Formats: []string{"opus"},
		Sidecars: []doctorSidecarProbe{
			{Endpoint: "session", OK: true, LatencyMs: 312},
			{Endpoint: "po-token", OK: true, LatencyMs: 12400},
			{Endpoint: "player-context", StatusCode: 502, Code: "player-context-failed", RetryAfterSeconds: 25,
				Error: &errorJSON{Code: "network", Message: "player-context server at http://127.0.0.1:4416/player-context returned HTTP 502 (player-context-failed)"}},
		},
	}
	renderDoctorHuman(env, rep, errors.New("player-context refused"))
	got := out.String()
	for _, want := range []string{
		"sidecar:  session ok (312 ms)",
		"sidecar:  po-token ok (12.4 s)",
		"sidecar:  player-context FAILED:",
		"retry in 25s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want it to contain %q", got, want)
		}
	}
}

// TestProbeSidecarsVerdictIsNotASidecarFailure covers a probed video that is
// simply dead: the sidecar answered, so the probe passes and the run stays
// healthy while the delivery check moves to the next candidate.
func TestProbeSidecarsVerdictIsNotASidecarFailure(t *testing.T) {
	verdict := &waxtap.SidecarResponseError{
		Label: "player-context server", Endpoint: "http://127.0.0.1:4416/player-context",
		StatusCode: 422, Code: waxtap.SidecarCodeVideoUnavailable, Details: "ERROR",
		Reason: "video unplayable: Video unavailable",
	}
	probes := probeSidecars(context.Background(), sidecarProviders{
		context: fakeContextProvider{err: verdict},
	}, "dummyVideo0", time.Minute)
	if len(probes) != 1 {
		t.Fatalf("probes = %+v, want one", probes)
	}
	p := probes[0]
	if !p.OK {
		t.Error("a relayed playability verdict is the sidecar working, not failing")
	}
	if p.Verdict != "video-unavailable" || p.StatusCode != 422 || p.Code != waxtap.SidecarCodeVideoUnavailable {
		t.Errorf("probe = %+v, want the verdict recorded beside the code", p)
	}
	if p.Error != nil {
		t.Errorf("probe.Error = %+v, want none: the sidecar did not fail", p.Error)
	}
	if !sidecarProbesHealthy(probes) || firstSidecarProbeError(probes) != nil {
		t.Error("a verdict must not make the run unhealthy: one dead demo video is not a sick sidecar")
	}
}

// TestProbeSidecarsBounded pins the probe budget: a hung sidecar cannot hold the
// command open past the run's web-context timeout.
func TestProbeSidecarsBounded(t *testing.T) {
	hung := hangingContextProvider{}
	start := time.Now()
	probes := probeSidecars(context.Background(), sidecarProviders{context: hung}, "dummyVideo0", 20*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("probe took %v, want it cut by the 20ms budget", d)
	}
	if len(probes) != 1 || probes[0].OK {
		t.Fatalf("probes = %+v, want one failure", probes)
	}
}

type hangingContextProvider struct{}

func (hangingContextProvider) ProvidePlayerContext(ctx context.Context, _ string) (potoken.PlayerContext, error) {
	<-ctx.Done()
	return potoken.PlayerContext{}, ctx.Err()
}

func TestCeilSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 0},
		{-time.Second, 0},
		{400 * time.Millisecond, 1}, // a sub-second wait must not read as "none stated"
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{25 * time.Second, 25},
	}
	for _, tc := range cases {
		if got := ceilSeconds(tc.in); got != tc.want {
			t.Errorf("ceilSeconds(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestDoctorHumanKeepsDeliveryEvidence covers a refusing sidecar beside a
// delivery check that passed: the run is unhealthy, but the extract/resolve/
// download lines still say what did work.
func TestDoctorHumanKeepsDeliveryEvidence(t *testing.T) {
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
	rep := &doctorReport{
		Formats: []string{"opus"}, Delivered: true, Healthy: false,
		VideoID: "jNQXAC9IVRw", Itag: 251, Bytes: 65536,
		Sidecars: []doctorSidecarProbe{
			{Endpoint: "player-context", StatusCode: 502, Code: "player-context-failed",
				Error: &errorJSON{Code: "network", Message: "player-context server refused"}},
		},
	}
	renderDoctorHuman(env, rep, errors.New("player-context server refused"))
	got := out.String()
	for _, want := range []string{"extract:  ok (jNQXAC9IVRw)", "resolve:  ok (itag 251)", "download: ok", "UNHEALTHY"} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want it to contain %q", got, want)
		}
	}
}

// The doctor document carries its caveat as a note too, so a consumer reading
// notes[] across commands does not have to special-case one string key.
func TestDoctorJSONNotes(t *testing.T) {
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}, notes: &noteCollector{}}
	env.note(noteWebSources, "a run note")
	rep := &doctorReport{Healthy: true, ForcedIOS: true, VideoID: "jNQXAC9IVRw", Bytes: 1 << 20}
	if err := emitDoctorJSON(env, rep, nil); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	// note stays what it was: the key is schema 3 and consumers read it.
	if s, _ := doc["note"].(string); !strings.Contains(s, "iOS media delivery") {
		t.Errorf("note = %v, want the caveat kept", doc["note"])
	}
	notes, ok := doc["notes"].([]any)
	if !ok || len(notes) != 2 {
		t.Fatalf("notes = %v, want the run note and the caveat", doc["notes"])
	}
	var codes []string
	for _, n := range notes {
		m, _ := n.(map[string]any)
		s, _ := m["code"].(string)
		codes = append(codes, s)
	}
	if !slices.Contains(codes, string(noteDoctorCaveat)) || !slices.Contains(codes, string(noteWebSources)) {
		t.Errorf("notes codes = %v, want both", codes)
	}

	// A clean report has no caveat, so the key stays absent.
	out.Reset()
	clean := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}, notes: &noteCollector{}}
	if err := emitDoctorJSON(clean, &doctorReport{Healthy: true, Full: true, VideoID: "jNQXAC9IVRw", Bytes: 8 << 20}, nil); err != nil {
		t.Fatal(err)
	}
	doc = nil
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["notes"]; ok {
		t.Errorf("notes = %v, want the key omitted on a clean run", doc["notes"])
	}
}
