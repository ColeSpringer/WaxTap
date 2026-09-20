package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/youtube"
	"github.com/spf13/cobra"
)

// doctorCandidates are known public videos used for extraction checks. doctor
// tries them in order so one unavailable video does not decide the result.
var doctorCandidates = []string{
	"jNQXAC9IVRw", // "Me at the zoo", the first YouTube video
	"rFejpH_tAHM", // dotGo 2015, Rob Pike
	"aqz-KE-bpKQ", // Big Buck Bunny (Blender, CC-BY)
}

// doctorByteProbe is how many bytes the cheap check reads to prove byte delivery.
const doctorByteProbe = 64 << 10

// doctorFullMinBytes is the least a --full check may deliver while claiming
// full-delivery health: the field failures start just under 1 MiB, so the probe
// wants comfortable margin past that boundary.
const doctorFullMinBytes = 2 << 20

// doctorFullCandidates orders candidates for --full so the first try crosses
// doctorFullMinBytes. The 19-second clip stays last as a connectivity fallback
// that cannot by itself prove full delivery.
var doctorFullCandidates = []string{
	"aqz-KE-bpKQ", // Big Buck Bunny (Blender, CC-BY), ~10 min
	"rFejpH_tAHM", // dotGo 2015, Rob Pike
	"jNQXAC9IVRw", // "Me at the zoo", too short to prove full delivery
}

func newDoctorCmd() *cobra.Command {
	var (
		full    bool
		videoID string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check extraction health (extract + resolve + byte read)",
		Long: "Runs a quick end-to-end health check: extract a known-good video,\n" +
			"resolve its best audio, and read a few KiB to prove byte delivery.\n" +
			"Use --full to download a whole track instead of a small range.\n\n" +
			"With sidecar URLs configured, the daemon behind them is asked for its\n" +
			"health first (WaxSeal's /ping, one round trip, shown with its reason),\n" +
			"then each endpoint is probed once (session, PO token, player-context),\n" +
			"so a cold daemon's first-call cost and any refusal code are visible.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			candidates := doctorCandidates
			if full {
				candidates = doctorFullCandidates
			}
			if videoID != "" {
				candidates = []string{videoID}
			}

			// Full describes the mode, set before any candidate runs: a --full
			// run whose candidates all fail must still report itself as one.
			rep := &doctorReport{Formats: media.OutputFormats(), Full: full, ForcedIOS: strings.EqualFold(env.cfg.client, "ios")}

			// Probe the sidecars first, on the candidate the check will use, so a
			// cold daemon's first-call cost lands here and the check that follows
			// runs warm. --video accepts a URL, but a sidecar is posted a bare
			// video ID, so the candidate is normalized first.
			probeID := candidates[0]
			if id, err := youtube.ExtractVideoID(probeID); err == nil {
				probeID = id
			}
			// The ping is set whenever any sidecar is; a daemon relaunching
			// its browser can hold this line for well over a minute.
			if env.sidecars.ping != nil {
				env.info("probing sidecars\n")
			}
			rep.Sidecars = probeSidecars(cmd.Context(), env.sidecars, probeID, env.cfg.webContextTimeout)

			var lastErr error
			for _, id := range candidates {
				env.info("checking %s\n", id)
				if err := runDoctorCheck(cmd.Context(), env, id, full, rep); err != nil {
					lastErr = err
					// A candidate that failed before a later one passed is the
					// signal --full is looking for: the run reports healthy, but
					// the long track did not deliver. Human mode prints the line
					// below; the record keeps it for --json too.
					rep.Attempts = append(rep.Attempts, doctorAttempt{VideoID: id, Error: *errorObject(err)})
					env.info("  %s: %s\n", id, friendlyError(err))
					continue
				}
				rep.Delivered = true
				rep.VideoID = id
				lastErr = nil
				break
			}
			// Healthy needs both halves. Delivered keeps the evidence apart so a
			// refusing sidecar does not erase a check that passed, and a probe
			// that refused decides the exit code once delivery itself succeeded.
			rep.Healthy = rep.Delivered && sidecarProbesHealthy(rep.Sidecars)
			if lastErr == nil {
				lastErr = firstSidecarProbeError(rep.Sidecars)
			}

			if env.jsonMode() {
				if err := emitDoctorJSON(env, rep, lastErr); err != nil {
					return err
				}
				// The JSON report already includes the failure. Preserve its exit
				// code without writing a second document.
				return alreadyRendered(lastErr)
			}
			renderDoctorHuman(env, rep, lastErr)
			return lastErr
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "download a complete track instead of a small byte range")
	cmd.Flags().StringVar(&videoID, "video", "", "check this specific video ID or URL instead of the built-in list")
	bindConfigFlags(cmd.Flags())
	bindNetworkFlags(cmd.Flags())
	bindPlayerExtractionFlags(cmd.Flags())
	return cmd
}

type doctorReport struct {
	Formats []string // audio formats the in-process engine can produce
	// Sidecars holds one probe per configured sidecar, run before the checks.
	Sidecars []doctorSidecarProbe
	// Delivered reports that a candidate extracted, resolved, and delivered
	// bytes. Healthy is that and every sidecar probe, so a refusing sidecar
	// makes the run unhealthy without erasing the delivery evidence.
	Delivered bool
	Healthy   bool
	VideoID   string
	Itag      int
	Bytes     int64
	Full      bool
	ForcedIOS bool
	// Attempts records candidates that failed before one succeeded. A run can be
	// healthy and still hold a failure worth reading.
	Attempts []doctorAttempt
}

// doctorAttempt is one candidate that failed during a check. Error is the
// {code, message} object every schemaVersion 3 document reports failures as.
type doctorAttempt struct {
	VideoID string    `json:"videoId"`
	Error   errorJSON `json:"error"`
}

// runDoctorCheck performs one candidate's check, filling rep on success.
func runDoctorCheck(ctx context.Context, env *appEnv, id string, full bool, rep *doctorReport) error {
	if full {
		// Stage the track in --temp-dir so the full check exercises cross-client
		// fallback and uses the configured filesystem.
		if base := env.cfg.tempDir; base != "" {
			if err := os.MkdirAll(base, 0o700); err != nil {
				return err
			}
		}
		dir, err := os.MkdirTemp(env.cfg.tempDir, "waxtap-doctor-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		res, err := env.client.Download(ctx, waxtap.Request{
			URL:         id,
			ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(filepath.Join(dir, "track"))},
		})
		if err != nil {
			return err
		}
		rep.Itag = res.SourceFormat.Itag
		rep.Bytes = res.OutputBytes
		return nil
	}

	rc, info, err := env.client.Stream(ctx, waxtap.Request{URL: id})
	if err != nil {
		return err
	}
	defer rc.Close()
	n, err := io.CopyN(io.Discard, rc, doctorByteProbe)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	rep.Itag = info.Format.Itag
	rep.Bytes = n
	return nil
}

func renderDoctorHuman(env *appEnv, rep *doctorReport, lastErr error) {
	renderSidecarProbes(env, rep.Sidecars)
	env.printf("engine:   pure-Go (formats: %s)\n", strings.Join(rep.Formats, ", "))
	if rep.Delivered {
		mode := "range read"
		if rep.Full {
			mode = "full download"
		}
		env.printf("extract:  ok (%s)\n", rep.VideoID)
		env.printf("resolve:  ok (itag %d)\n", rep.Itag)
		env.printf("download: ok (%s, %s)\n", mode, humanBytes(rep.Bytes))
	}
	if rep.Healthy {
		env.printf("\nhealthy\n")
		if note := doctorNote(rep); note != "" {
			env.printf("note: %s\n", note)
		}
		return
	}
	env.printf("\nUNHEALTHY: %s\n", friendlyError(lastErr))
}

// doctorIOSBestEffortNote warns when a forced-iOS range check passes without
// verifying a complete download.
func doctorIOSBestEffortNote(rep *doctorReport) string {
	if !rep.Healthy || rep.Full || !rep.ForcedIOS {
		return ""
	}
	return "iOS media delivery is unreliable in current testing, even on short clips. Omit --client for reliable audio. This range check passed, but a full download may still fail. Verify with `doctor --client ios --full --video <long-id>`"
}

// doctorFullSizeNote reports a --full check that passed on a track too small to
// have exercised a full-length delivery. Both sizes are rendered by humanBytes,
// so the sentence cannot drift from the constant it enforces.
func doctorFullSizeNote(rep *doctorReport) string {
	if !rep.Healthy || !rep.Full || rep.Bytes >= doctorFullMinBytes {
		return ""
	}
	return "the delivered stream is only " + humanBytes(rep.Bytes) + ", under the " + humanBytes(doctorFullMinBytes) +
		" this check wants; it did not exercise a full-length delivery, so larger downloads may still fail. Retry, or use --video with a longer video"
}

// doctorNote picks the single note the report carries. The two notes are
// mutually exclusive on rep.Full: the iOS caveat is for range checks, the size
// caveat for --full runs.
func doctorNote(rep *doctorReport) string {
	if note := doctorIOSBestEffortNote(rep); note != "" {
		return note
	}
	return doctorFullSizeNote(rep)
}

func emitDoctorJSON(env *appEnv, rep *doctorReport, lastErr error) error {
	// The caveat is recorded as a note as well as kept under its own key: note
	// is schema 3 and consumers read it, and notes[] is what every other
	// document carries, so a reader of notes[] needs no special case here.
	if note := doctorNote(rep); note != "" {
		env.note(noteDoctorCaveat, "%s", note)
	}
	out := struct {
		SchemaVersion int                  `json:"schemaVersion"`
		Healthy       bool                 `json:"healthy"`
		Engine        string               `json:"engine"`
		Formats       []string             `json:"formats"`
		VideoID       string               `json:"videoId,omitempty"`
		Itag          int                  `json:"itag,omitempty"`
		Bytes         int64                `json:"bytes,omitempty"`
		FullDownload  bool                 `json:"fullDownload"`
		Note          string               `json:"note,omitempty"`
		Attempts      []doctorAttempt      `json:"attempts,omitempty"`
		Sidecars      []doctorSidecarProbe `json:"sidecars,omitempty"`
		Error         *errorJSON           `json:"error,omitempty"`
		Notes         []noteJSON           `json:"notes,omitempty"`
	}{
		SchemaVersion: schemaVersion,
		Healthy:       rep.Healthy,
		Engine:        "waxflow",
		Formats:       rep.Formats,
		VideoID:       rep.VideoID,
		Itag:          rep.Itag,
		Bytes:         rep.Bytes,
		FullDownload:  rep.Full,
		Note:          doctorNote(rep),
		Attempts:      rep.Attempts,
		Sidecars:      rep.Sidecars,
		Notes:         env.notesJSON(),
	}
	if lastErr != nil {
		out.Error = errorObject(lastErr)
	}
	return env.emitJSON(out)
}

// doctorProbePing names the ping's entry; the endpoint probes are named by
// their endpoint.
const doctorProbePing = "ping"

// doctorSidecarProbe is one configured sidecar's probe.
type doctorSidecarProbe struct {
	Endpoint string `json:"endpoint"` // "ping", "session", "po-token", "player-context"
	// Via, on the ping, names the sidecar whose URL it was derived from
	// ("session", "po-token", "player-context"), since the ping goes to one of
	// the three by preference where the other entries' names say which.
	Via               string `json:"via,omitempty"`
	OK                bool   `json:"ok"`
	LatencyMs         int64  `json:"latencyMs"`
	StatusCode        int    `json:"statusCode,omitempty"`
	Code              string `json:"code,omitempty"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
	// Verdict names the availability class a refusal relayed about the video
	// (for example "video-unavailable"). It is set only on a probe that is OK:
	// the sidecar answered, the video is what refused.
	Verdict string `json:"verdict,omitempty"`
	// Probe, Reason, and BrowserRelaunched are what the ping's health body
	// said, in WaxSeal's words: the scope the daemon checked ("tenant" or
	// "daemon"), its reason ("ok", "no-session", "busy", "probe-failed"), and
	// whether the probe found the browser gone and relaunched it. Only the
	// ping sets them, and a daemon that answered without a health body leaves
	// them empty.
	Probe             string     `json:"probe,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	BrowserRelaunched bool       `json:"browserRelaunched,omitempty"`
	Error             *errorJSON `json:"error,omitempty"`
	// err is the failure Error renders, kept so the command can return the
	// refusal itself and let it decide the exit code.
	err error
}

// probeSidecars asks each configured sidecar once, in the order ping, session,
// po-token, player-context, the three endpoint probes each bounded by budget
// (the run's web-context timeout, the same budget the library gives one
// attested handoff).
//
// The ping is the cheap question first. WaxSeal answers GET /ping?strict=true
// from one page round trip without attesting or minting, and says why when it
// is not live (no-session, busy, or a loss the probe confirmed), where the
// browser-backed probes that follow can only relay a refusal. A keyed daemon
// answers a keyless ping at daemon scope, so a missing key shows there as
// "daemon" ahead of the 401 the others report. A daemon with no /ping is not a
// sick one: its answer is recorded and the probes that follow ask it what it
// does offer. The ping runs under no handoff deadline: it neither retries nor
// sleeps, so the library's own request bound covers the whole call, and that
// bound is sized for WaxSeal's probe worst case, a wedged browser found, torn
// down, and relaunched, which the handoff budget would cut short and so lose
// the verdict the ping exists to fetch.
//
// The order of the three follows WaxSeal's separation windows: it keeps a
// cache-miss mint 12 s clear of the last session establishment and a served
// context 12 s from the last mint, and contexts served earlier do not extend
// the window. Paying those waits inside the probes leaves the doctor
// download's own context immediate and its GVS mint a cache hit, so the probe
// latencies, not the download, carry the first-call cost. The token probe
// therefore binds a GVS-scope request to the session probe's visitorData when
// that probe delivered one (the mint the download needs), else asks for a
// player-scope token on the video ID.
func probeSidecars(ctx context.Context, s sidecarProviders, videoID string, budget time.Duration) []doctorSidecarProbe {
	var probes []doctorSidecarProbe
	var visitorData string

	// probe runs one check under bound and returns its entry, classified.
	// Bounding an endpoint probe the way the library bounds one attested
	// handoff means wedged sidecars cannot hold the command open for minutes.
	// The wait a refusal asks for is paid inside this bound, which is why the
	// latency is reported: it is the cost a first download would pay.
	probe := func(endpoint string, bound time.Duration, run func(context.Context) error) doctorSidecarProbe {
		pctx, cancel := withBudget(ctx, bound)
		defer cancel()
		start := time.Now()
		err := run(pctx)
		p := doctorSidecarProbe{Endpoint: endpoint, LatencyMs: time.Since(start).Milliseconds()}
		if err == nil {
			p.OK = true
			return p
		}
		if sre, ok := errors.AsType[*waxtap.SidecarResponseError](err); ok {
			p.StatusCode = sre.StatusCode
			p.Code = sre.Code
			p.RetryAfterSeconds = ceilSeconds(sre.RetryAfter)
		}
		// A relayed playability verdict is the sidecar working: its browser
		// reached YouTube and reported what the video is. Only the video is
		// unusable, and the check below moves on to the next candidate, so the
		// probe records the verdict without condemning the sidecar.
		if exitCodeFor(err) == 3 {
			p.OK = true
			p.Verdict = errorObject(err).Code
			return p
		}
		p.Error = errorObject(err)
		p.err = err
		return p
	}

	if s.ping != nil {
		var health waxtap.SidecarHealth
		p := probe(doctorProbePing, 0, func(ctx context.Context) error {
			h, err := s.ping(ctx)
			health = h
			return err
		})
		p.Via = s.pingVia
		p.Probe, p.Reason, p.BrowserRelaunched = health.Probe, health.Reason, health.BrowserRelaunched
		// A daemon that offers no /ping, or a proxy in front of one that routes
		// only the endpoints, says so with a status that names a missing route.
		// That says nothing about its health, so the entry keeps the status and
		// the code and drops the failure, and drops the wait a proxy may have
		// tacked on, which a route that does not exist cannot mean.
		if pingNotOffered(p.StatusCode) {
			p.OK, p.Error, p.err, p.RetryAfterSeconds = true, nil, nil, 0
		}
		probes = append(probes, p)
	}
	if s.session != nil {
		probes = append(probes, probe("session", budget, func(ctx context.Context) error {
			sess, err := s.session.ProvideSession(ctx)
			if err != nil {
				return err
			}
			if sess.VisitorData == "" {
				return errors.New("session endpoint returned an empty visitorData")
			}
			visitorData = sess.VisitorData
			return nil
		}))
	}
	if s.token != nil {
		probes = append(probes, probe("po-token", budget, func(ctx context.Context) error {
			req := potoken.Request{Scope: potoken.ScopeGVS, VisitorData: visitorData}
			if visitorData == "" {
				// No adopted session to bind to; a player-scope token still proves
				// the minter answers.
				req = potoken.Request{Scope: potoken.ScopePlayer, VideoID: videoID}
			}
			resp, err := s.token.ProvidePOToken(ctx, req)
			if err != nil {
				return err
			}
			if resp.Token == "" {
				return errors.New("PO-token server returned an empty token")
			}
			return nil
		}))
	}
	if s.context != nil {
		probes = append(probes, probe("player-context", budget, func(ctx context.Context) error {
			_, err := s.context.ProvidePlayerContext(ctx, videoID)
			return err
		}))
	}
	return probes
}

// pingNotOffered reports a status that says the route is not served here: not
// found, the method not allowed, gone, or not implemented. None can mean the
// daemon is unhealthy or the key wrong, which a 401 or 403 can, so those stay
// failures.
func pingNotOffered(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

// notOffered reports a ping the daemon answered with a status pingNotOffered
// names, recorded ok for it.
func (p doctorSidecarProbe) notOffered() bool {
	return p.Endpoint == doctorProbePing && p.OK && pingNotOffered(p.StatusCode)
}

// withBudget bounds one probe. A non-positive budget adds no deadline, matching
// how a zero Timeouts field behaves everywhere else.
func withBudget(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// ceilSeconds renders a wait as whole seconds, rounding a positive sub-second
// wait up to 1 rather than down to 0, which both renderers read as "no wait was
// stated".
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// sidecarProbesHealthy reports whether every probe succeeded.
func sidecarProbesHealthy(probes []doctorSidecarProbe) bool {
	for _, p := range probes {
		if !p.OK {
			return false
		}
	}
	return true
}

// firstSidecarProbeError returns the error the run reports for a failed probe,
// so a run whose delivery succeeded still exits on the refusal that made it
// unhealthy. An endpoint probe's failure outranks the ping's: the endpoints are
// what a download hits, and the ping line already shows its own, so the run's
// error and exit code name what a download would meet.
func firstSidecarProbeError(probes []doctorSidecarProbe) error {
	var ping error
	for _, p := range probes {
		switch {
		case p.OK:
		case p.Endpoint == doctorProbePing:
			ping = p.err
		default:
			return p.err
		}
	}
	return ping
}

// humanLatency renders a probe latency: milliseconds under a second, seconds
// above. humanDuration is not reused; it renders track lengths as m:ss.
func humanLatency(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}
	return fmt.Sprintf("%.1f s", float64(ms)/1000)
}

// renderSidecarProbes prints one line per probe, before the engine line.
func renderSidecarProbes(env *appEnv, probes []doctorSidecarProbe) {
	for _, p := range probes {
		switch {
		case !p.OK:
			line := fmt.Sprintf("sidecar:  %s FAILED: %s", p.Endpoint, p.Error.Message)
			if detail := pingFailureDetail(p); detail != "" {
				line += " (" + detail + ")"
			}
			if p.RetryAfterSeconds > 0 {
				line += fmt.Sprintf("; retry in %ds", p.RetryAfterSeconds)
			}
			env.printf("%s\n", line)
		case p.Verdict != "":
			env.printf("sidecar:  %s ok (%s, reported %s for the probed video)\n", p.Endpoint, humanLatency(p.LatencyMs), p.Verdict)
		case p.notOffered():
			env.printf("sidecar:  %s not offered (HTTP %d, %s)\n", p.Endpoint, p.StatusCode, humanLatency(p.LatencyMs))
		default:
			env.printf("sidecar:  %s ok (%s%s)\n", p.Endpoint, humanLatency(p.LatencyMs), healthDetail(p))
		}
	}
}

// healthDetail renders what a ping's health body said, after the latency: the
// daemon's scope and reason in its own words ("tenant ok", "daemon ok",
// "tenant no-session"), a reason alone as "reason busy" (a daemon that
// predates the scope field), a scope alone as "tenant probe", then a relaunch.
// A daemon that answered without a health body adds nothing.
func healthDetail(p doctorSidecarProbe) string {
	var parts []string
	switch {
	case p.Probe != "" && p.Reason != "":
		parts = append(parts, p.Probe+" "+p.Reason)
	case p.Reason != "":
		parts = append(parts, "reason "+p.Reason)
	case p.Probe != "":
		parts = append(parts, p.Probe+" probe")
	}
	if p.BrowserRelaunched {
		parts = append(parts, "browser relaunched")
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

// pingFailureDetail is what a failed ping's health body adds after its
// message: the scope the daemon checked and a relaunch it made, which the
// daemon's own account need not say. The reason is already the message's code.
func pingFailureDetail(p doctorSidecarProbe) string {
	var parts []string
	if p.Probe != "" {
		parts = append(parts, p.Probe+" probe")
	}
	if p.BrowserRelaunched {
		parts = append(parts, "browser relaunched")
	}
	return strings.Join(parts, ", ")
}
