package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
			"With sidecar URLs configured, each is probed once first (session, PO\n" +
			"token, player-context), so a cold daemon's first-call cost and any\n" +
			"refusal code are visible.",
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
	}
	if lastErr != nil {
		out.Error = errorObject(lastErr)
	}
	return env.emitJSON(out)
}

// doctorSidecarProbe is one configured sidecar's probe.
type doctorSidecarProbe struct {
	Endpoint          string `json:"endpoint"` // "session", "player-context", "po-token"
	OK                bool   `json:"ok"`
	LatencyMs         int64  `json:"latencyMs"`
	StatusCode        int    `json:"statusCode,omitempty"`
	Code              string `json:"code,omitempty"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
	// Verdict names the availability class a refusal relayed about the video
	// (for example "video-unavailable"). It is set only on a probe that is OK:
	// the sidecar answered, the video is what refused.
	Verdict string     `json:"verdict,omitempty"`
	Error   *errorJSON `json:"error,omitempty"`
	// err is the failure Error renders, kept so the command can return the
	// refusal itself and let it decide the exit code.
	err error
}

// probeSidecars calls each configured sidecar once, in the order session,
// po-token, player-context, each bounded by budget (the run's web-context
// timeout, the same budget the library gives one attested handoff).
//
// That order follows WaxSeal's separation windows: it keeps a cache-miss mint
// 12 s clear of the last session establishment and a served context 12 s from
// the last mint, and contexts served earlier do not extend the window. Paying
// those waits inside the probes leaves the doctor download's own context
// immediate and its GVS mint a cache hit, so the probe latencies, not the
// download, carry the first-call cost. The token probe therefore binds a
// GVS-scope request to the session probe's visitorData when that probe delivered
// one (the mint the download needs), else asks for a player-scope token on the
// video ID.
func probeSidecars(ctx context.Context, s sidecarProviders, videoID string, budget time.Duration) []doctorSidecarProbe {
	var probes []doctorSidecarProbe
	var visitorData string

	probe := func(endpoint string, run func(context.Context) error) {
		// Bound each probe the way the library bounds one attested handoff, so
		// three wedged sidecars cannot hold the command open for minutes. The
		// wait a refusal asks for is paid inside this budget, which is why the
		// latency is reported: it is the cost a first download would pay.
		pctx, cancel := withBudget(ctx, budget)
		defer cancel()
		start := time.Now()
		err := run(pctx)
		p := doctorSidecarProbe{Endpoint: endpoint, LatencyMs: time.Since(start).Milliseconds()}
		if err == nil {
			p.OK = true
			probes = append(probes, p)
			return
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
			probes = append(probes, p)
			return
		}
		p.Error = errorObject(err)
		p.err = err
		probes = append(probes, p)
	}

	if s.session != nil {
		probe("session", func(ctx context.Context) error {
			sess, err := s.session.ProvideSession(ctx)
			if err != nil {
				return err
			}
			if sess.VisitorData == "" {
				return errors.New("session endpoint returned an empty visitorData")
			}
			visitorData = sess.VisitorData
			return nil
		})
	}
	if s.token != nil {
		probe("po-token", func(ctx context.Context) error {
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
		})
	}
	if s.context != nil {
		probe("player-context", func(ctx context.Context) error {
			_, err := s.context.ProvidePlayerContext(ctx, videoID)
			return err
		})
	}
	return probes
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

// firstSidecarProbeError returns the error of the first failed probe, so a run
// whose delivery succeeded still exits on the refusal that made it unhealthy.
func firstSidecarProbeError(probes []doctorSidecarProbe) error {
	for _, p := range probes {
		if !p.OK {
			return p.err
		}
	}
	return nil
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
		if p.OK {
			if p.Verdict != "" {
				env.printf("sidecar:  %s ok (%s, reported %s for the probed video)\n", p.Endpoint, humanLatency(p.LatencyMs), p.Verdict)
				continue
			}
			env.printf("sidecar:  %s ok (%s)\n", p.Endpoint, humanLatency(p.LatencyMs))
			continue
		}
		line := fmt.Sprintf("sidecar:  %s FAILED: %s", p.Endpoint, p.Error.Message)
		if p.RetryAfterSeconds > 0 {
			line += fmt.Sprintf("; retry in %ds", p.RetryAfterSeconds)
		}
		env.printf("%s\n", line)
	}
}
