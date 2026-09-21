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

// TestProbeSidecarsPing covers the health question doctor asks before it pays
// for a proof: it runs first, its entry carries what the daemon said, a loss
// the daemon confirmed makes the run unhealthy on that error, and a daemon
// with no /ping is not a sick one.
func TestProbeSidecarsPing(t *testing.T) {
	pingWith := func(h waxtap.SidecarHealth, err error) func(context.Context) (waxtap.SidecarHealth, error) {
		return func(context.Context) (waxtap.SidecarHealth, error) { return h, err }
	}

	t.Run("live tenant answers first", func(t *testing.T) {
		probes := probeSidecars(context.Background(), sidecarProviders{
			ping:    pingWith(waxtap.SidecarHealth{OK: true, Probe: "tenant", Reason: "ok"}, nil),
			session: fakeSessionProvider{sess: potoken.Session{VisitorData: "vd"}},
		}, "dummyVideo0", time.Minute)
		if len(probes) != 2 || probes[0].Endpoint != "ping" || probes[1].Endpoint != "session" {
			t.Fatalf("probes = %+v, want the ping ahead of the session probe", probes)
		}
		p := probes[0]
		if !p.OK || p.Probe != "tenant" || p.Reason != "ok" || p.BrowserRelaunched || p.Error != nil {
			t.Errorf("ping = %+v, want ok with the daemon's scope and reason", p)
		}
	})

	t.Run("benign window with a relaunch stays healthy", func(t *testing.T) {
		probes := probeSidecars(context.Background(), sidecarProviders{
			ping: pingWith(waxtap.SidecarHealth{Probe: "tenant", Reason: "no-session", Error: "no attested session", BrowserRelaunched: true}, nil),
		}, "dummyVideo0", time.Minute)
		if len(probes) != 1 || !probes[0].OK || probes[0].Reason != "no-session" || !probes[0].BrowserRelaunched {
			t.Fatalf("probes = %+v, want an ok ping carrying no-session and the relaunch", probes)
		}
		if !sidecarProbesHealthy(probes) {
			t.Error("a benign window is the daemon's own 200 under strict; the run stays healthy")
		}
	})

	t.Run("an endpoint probe's failure names the run's error ahead of the ping's", func(t *testing.T) {
		loss := &waxtap.SidecarResponseError{Label: "sidecar health endpoint", Endpoint: "http://127.0.0.1:4416/ping?strict=true", StatusCode: 503, Code: "probe-failed"}
		refused := &waxtap.SidecarResponseError{Label: "session endpoint", Endpoint: "http://127.0.0.1:4416/session", StatusCode: 401, Code: "unauthorized"}
		probes := probeSidecars(context.Background(), sidecarProviders{
			ping:    pingWith(waxtap.SidecarHealth{Reason: "probe-failed"}, loss),
			session: fakeSessionProvider{err: refused},
		}, "dummyVideo0", time.Minute)
		if len(probes) != 2 || probes[0].OK || probes[1].OK {
			t.Fatalf("probes = %+v, want both failed", probes)
		}
		// The session endpoint is what a download hits, so its refusal, and
		// its exit code, is the run's; the ping line above already shows the
		// ping's own.
		if err := firstSidecarProbeError(probes); err != refused {
			t.Errorf("firstSidecarProbeError = %v, want the session probe's refusal ahead of the ping's", err)
		}
	})

	t.Run("confirmed loss fails the run on its error", func(t *testing.T) {
		loss := &waxtap.SidecarResponseError{Label: "sidecar health endpoint", Endpoint: "http://127.0.0.1:4416/ping?strict=true", StatusCode: 503, Code: "probe-failed", Reason: "no browser answers and none could be launched"}
		probes := probeSidecars(context.Background(), sidecarProviders{
			ping:    pingWith(waxtap.SidecarHealth{Probe: "daemon", Reason: "probe-failed", Error: "no browser answers and none could be launched"}, loss),
			session: fakeSessionProvider{sess: potoken.Session{VisitorData: "vd"}},
		}, "dummyVideo0", time.Minute)
		if len(probes) != 2 {
			t.Fatalf("probes = %+v, want the ping and the session probe", probes)
		}
		p := probes[0]
		if p.OK || p.StatusCode != 503 || p.Code != "probe-failed" || p.Probe != "daemon" || p.Reason != "probe-failed" || p.Error == nil || p.Error.Code != "network" {
			t.Errorf("ping = %+v, want the 503 with its reason and a network-class error", p)
		}
		if sidecarProbesHealthy(probes) {
			t.Error("a loss the daemon confirmed must make the run unhealthy")
		}
		if err := firstSidecarProbeError(probes); err != loss {
			t.Errorf("firstSidecarProbeError = %v, want the ping's refusal, which decides the exit code", err)
		}
	})

	t.Run("no /ping is not a failure", func(t *testing.T) {
		// Every answer that says "no such route here", with a wait a proxy
		// might tack on, which a route that does not exist cannot mean.
		for _, status := range []int{404, 405, 410, 501} {
			missing := &waxtap.SidecarResponseError{Label: "sidecar health endpoint", Endpoint: "http://127.0.0.1:4416/ping?strict=true", StatusCode: status, Code: "not-found", RetryAfter: 5 * time.Second}
			probes := probeSidecars(context.Background(), sidecarProviders{
				ping:    pingWith(waxtap.SidecarHealth{}, missing),
				pingVia: "po-token",
				token:   &fakeTokenProvider{tok: "tok"},
			}, "dummyVideo0", time.Minute)
			if len(probes) != 2 {
				t.Fatalf("probes = %+v, want the ping and the token probe", probes)
			}
			p := probes[0]
			if !p.OK || p.StatusCode != status || p.Error != nil || p.err != nil || p.RetryAfterSeconds != 0 {
				t.Errorf("HTTP %d: ping = %+v, want it recorded ok with the status, no error, and no wait: the daemon offers no /ping, and the token probe asks what it does offer", status, p)
			}
			if p.Via != "po-token" {
				t.Errorf("ping via = %q, want the sidecar whose URL it was derived from", p.Via)
			}
			if !sidecarProbesHealthy(probes) || firstSidecarProbeError(probes) != nil {
				t.Errorf("HTTP %d: a daemon without /ping must not fail the run", status)
			}
		}
	})

	t.Run("a 403 on /ping is still a failure", func(t *testing.T) {
		walled := &waxtap.SidecarResponseError{Label: "sidecar health endpoint", Endpoint: "http://127.0.0.1:4416/ping?strict=true", StatusCode: 403, Code: "forbidden"}
		probes := probeSidecars(context.Background(), sidecarProviders{ping: pingWith(waxtap.SidecarHealth{}, walled)}, "dummyVideo0", time.Minute)
		if len(probes) != 1 || probes[0].OK {
			t.Fatalf("probes = %+v, want a failure: a 403 can mean a wrong key, which only the ping said cheaply", probes)
		}
	})

	t.Run("unreachable daemon fails the run", func(t *testing.T) {
		down := &waxtap.SidecarError{Label: "sidecar health endpoint", Endpoint: "http://127.0.0.1:4416/ping?strict=true", Err: errors.New("connection refused")}
		probes := probeSidecars(context.Background(), sidecarProviders{ping: pingWith(waxtap.SidecarHealth{}, down)}, "dummyVideo0", time.Minute)
		if len(probes) != 1 || probes[0].OK || probes[0].Error == nil || probes[0].Error.Code != "network" {
			t.Fatalf("probes = %+v, want one network-class failure", probes)
		}
	})
}

// The ping runs without the handoff deadline the three probes share. It
// neither retries nor sleeps, so the library's own request bound, sized for
// WaxSeal's probe worst case, covers the whole call, where the 60 s handoff
// budget would cut a wedged daemon's answer short and lose the verdict.
func TestProbeSidecarsPingOutlivesTheHandoffBudget(t *testing.T) {
	var pingBounded, sessionBounded bool
	probes := probeSidecars(context.Background(), sidecarProviders{
		ping: func(ctx context.Context) (waxtap.SidecarHealth, error) {
			_, pingBounded = ctx.Deadline()
			return waxtap.SidecarHealth{OK: true, Probe: "tenant", Reason: "ok"}, nil
		},
		session: deadlineSessionProvider{bounded: &sessionBounded},
	}, "dummyVideo0", 20*time.Millisecond)
	if len(probes) != 2 || !probes[0].OK || !probes[1].OK {
		t.Fatalf("probes = %+v, want both ok", probes)
	}
	if pingBounded {
		t.Error("the ping ran under the handoff deadline, which cuts WaxSeal's worst-case probe short")
	}
	if !sessionBounded {
		t.Error("the session probe must keep the handoff deadline")
	}
}

// deadlineSessionProvider records whether its context carried a deadline.
type deadlineSessionProvider struct{ bounded *bool }

func (d deadlineSessionProvider) ProvideSession(ctx context.Context) (potoken.Session, error) {
	_, *d.bounded = ctx.Deadline()
	return potoken.Session{VisitorData: "vd"}, nil
}

// TestDoctorHumanPingLines pins the one line each ping outcome renders as.
func TestDoctorHumanPingLines(t *testing.T) {
	cases := []struct {
		name  string
		probe doctorSidecarProbe
		want  string
	}{
		{"tenant ok", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 12, Probe: "tenant", Reason: "ok"}, "sidecar:  ping ok (12 ms, tenant ok)"},
		{"daemon scope", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 9, Probe: "daemon", Reason: "ok"}, "sidecar:  ping ok (9 ms, daemon ok)"},
		{"benign with a relaunch", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 2300, Probe: "tenant", Reason: "no-session", BrowserRelaunched: true}, "sidecar:  ping ok (2.3 s, tenant no-session, browser relaunched)"},
		{"older daemon without a probe scope", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 12, Reason: "busy"}, "sidecar:  ping ok (12 ms, reason busy)"},
		{"answered without a health body", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 3}, "sidecar:  ping ok (3 ms)"},
		{"probe scope without a reason word", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 12, Probe: "tenant"}, "sidecar:  ping ok (12 ms, tenant probe)"},
		{"not offered", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 2, StatusCode: 404, Code: "not-found"}, "sidecar:  ping not offered (HTTP 404, 2 ms)"},
		{"not offered as a method", doctorSidecarProbe{Endpoint: "ping", OK: true, LatencyMs: 2, StatusCode: 405}, "sidecar:  ping not offered (HTTP 405, 2 ms)"},
		// A verdict probe keeps its own line whatever status it carries.
		{"verdict", doctorSidecarProbe{Endpoint: "player-context", OK: true, LatencyMs: 5, StatusCode: 422, Code: "video-unavailable", Verdict: "video-unavailable"}, "sidecar:  player-context ok (5 ms, reported video-unavailable for the probed video)"},
		{"confirmed loss", doctorSidecarProbe{Endpoint: "ping", LatencyMs: 40000, StatusCode: 503, Code: "probe-failed", Probe: "tenant", Reason: "probe-failed",
			Error: &errorJSON{Code: "network", Message: "sidecar health endpoint at http://127.0.0.1:4416/ping returned HTTP 503 (probe-failed): no browser answers and none could be launched"}},
			"sidecar:  ping FAILED: sidecar health endpoint at http://127.0.0.1:4416/ping returned HTTP 503 (probe-failed): no browser answers and none could be launched (tenant probe)"},
		// What the daemon checked and whether it relaunched the browser matter
		// most on a failure, and go after the message, ahead of a stated wait.
		{"confirmed loss after a relaunch, with a wait", doctorSidecarProbe{Endpoint: "ping", LatencyMs: 40000, StatusCode: 503, Code: "probe-failed", Probe: "daemon", Reason: "probe-failed", BrowserRelaunched: true, RetryAfterSeconds: 5,
			Error: &errorJSON{Code: "network", Message: "M"}},
			"sidecar:  ping FAILED: M (daemon probe, browser relaunched); retry in 5s"},
		{"unreachable, nothing decoded", doctorSidecarProbe{Endpoint: "ping", LatencyMs: 3, Error: &errorJSON{Code: "network", Message: "M"}}, "sidecar:  ping FAILED: M"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
			renderSidecarProbes(env, []doctorSidecarProbe{tc.probe})
			if got := strings.TrimSuffix(out.String(), "\n"); got != tc.want {
				t.Errorf("line = %q\nwant   %q", got, tc.want)
			}
		})
	}
}

// The ping's entry carries the daemon's words under their own keys, and only
// when the daemon said them, so the other probes' entries are unchanged.
func TestDoctorJSONPingEntry(t *testing.T) {
	decode := func(rep *doctorReport) []any {
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
		arr, _ := m["sidecars"].([]any)
		return arr
	}
	arr := decode(&doctorReport{Healthy: true, Sidecars: []doctorSidecarProbe{
		{Endpoint: "ping", OK: true, LatencyMs: 12, Via: "session", Probe: "tenant", Reason: "no-session", BrowserRelaunched: true, Keyed: true},
		{Endpoint: "ping", OK: true, LatencyMs: 3},
		{Endpoint: "session", OK: true, LatencyMs: 300},
	}})
	if len(arr) != 3 {
		t.Fatalf("sidecars = %v, want three entries", arr)
	}
	first, _ := arr[0].(map[string]any)
	if first["via"] != "session" || first["probe"] != "tenant" || first["reason"] != "no-session" || first["browserRelaunched"] != true || first["keyed"] != true {
		t.Errorf("sidecars[0] = %v, want via, probe, reason, browserRelaunched, and keyed", first)
	}
	for i := 1; i < 3; i++ {
		entry, _ := arr[i].(map[string]any)
		for _, key := range []string{"via", "probe", "reason", "browserRelaunched", "keyed"} {
			if _, ok := entry[key]; ok {
				t.Errorf("sidecars[%d] = %v, want %q omitted when the daemon said nothing", i, entry, key)
			}
		}
	}
}

// The ping's verdict is one word, so a consumer reads it instead of deriving
// it from ok, statusCode, and the health fields.
func TestDoctorPingStatus(t *testing.T) {
	cases := []struct {
		name  string
		probe doctorSidecarProbe
		want  string
	}{
		{"health body", doctorSidecarProbe{Endpoint: "ping", OK: true, Probe: "tenant", Reason: "ok"}, "healthy"},
		{"benign window", doctorSidecarProbe{Endpoint: "ping", OK: true, Probe: "tenant", Reason: "busy"}, "healthy"},
		{"no health body", doctorSidecarProbe{Endpoint: "ping", OK: true}, "answered"},
		{"missing route", doctorSidecarProbe{Endpoint: "ping", OK: true, StatusCode: 404}, "not-offered"},
		{"failed", doctorSidecarProbe{Endpoint: "ping", Probe: "tenant", Reason: "probe-failed"}, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pingStatus(tc.probe); got != tc.want {
				t.Errorf("pingStatus = %q, want %q", got, tc.want)
			}
		})
	}
	// Only the ping carries one: an endpoint probe's entry is unchanged.
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
	if err := emitDoctorJSON(env, &doctorReport{Healthy: true, Sidecars: []doctorSidecarProbe{
		{Endpoint: "ping", OK: true, Probe: "tenant", Reason: "ok", Status: "healthy"},
		{Endpoint: "session", OK: true},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	arr, _ := m["sidecars"].([]any)
	if first, _ := arr[0].(map[string]any); first["status"] != "healthy" {
		t.Errorf("ping status = %v, want healthy", arr[0])
	}
	if second, _ := arr[1].(map[string]any); second["status"] != nil {
		t.Errorf("session entry = %v, want no status", arr[1])
	}
}

// The run's --api-key and the daemon's own keying are reported when they
// disagree, in either direction, and a daemon that said nothing about its
// keying produces neither caveat.
func TestDoctorKeyingCaveats(t *testing.T) {
	cases := []struct {
		name   string
		apiKey string
		probe  doctorSidecarProbe
		want   string
	}{
		{"keyed daemon, no key", "", doctorSidecarProbe{Endpoint: "ping", OK: true, Probe: "daemon", Keyed: true}, "no --api-key was given"},
		{"keyless daemon, a key", "bogus", doctorSidecarProbe{Endpoint: "ping", OK: true, Probe: "tenant"}, "not checked"},
		{"keyed daemon with a key", "k", doctorSidecarProbe{Endpoint: "ping", OK: true, Probe: "tenant", Keyed: true}, ""},
		{"no health body", "k", doctorSidecarProbe{Endpoint: "ping", OK: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			env := &appEnv{out: io.Discard, errOut: &errOut, cfg: &appConfig{apiKey: tc.apiKey}, notes: &noteCollector{}}
			noteSidecarKeying(env, []doctorSidecarProbe{tc.probe})
			notes := env.notesJSON()
			if tc.want == "" {
				if len(notes) != 0 {
					t.Fatalf("notes = %v, want none", notes)
				}
				return
			}
			if len(notes) != 1 || notes[0].Code != string(noteDoctorCaveat) || !strings.Contains(notes[0].Detail, tc.want) {
				t.Fatalf("notes = %v, want one doctor-caveat containing %q", notes, tc.want)
			}
		})
	}
}
