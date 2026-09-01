package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/media"
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
			"Use --full to download a whole track instead of a small range.",
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
				rep.Healthy = true
				rep.VideoID = id
				lastErr = nil
				break
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
	Formats   []string // audio formats the in-process engine can produce
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
	env.printf("engine:   pure-Go (formats: %s)\n", strings.Join(rep.Formats, ", "))
	if rep.Healthy {
		mode := "range read"
		if rep.Full {
			mode = "full download"
		}
		env.printf("extract:  ok (%s)\n", rep.VideoID)
		env.printf("resolve:  ok (itag %d)\n", rep.Itag)
		env.printf("download: ok (%s, %s)\n", mode, humanBytes(rep.Bytes))
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
		SchemaVersion int             `json:"schemaVersion"`
		Healthy       bool            `json:"healthy"`
		Engine        string          `json:"engine"`
		Formats       []string        `json:"formats"`
		VideoID       string          `json:"videoId,omitempty"`
		Itag          int             `json:"itag,omitempty"`
		Bytes         int64           `json:"bytes,omitempty"`
		FullDownload  bool            `json:"fullDownload"`
		Note          string          `json:"note,omitempty"`
		Attempts      []doctorAttempt `json:"attempts,omitempty"`
		Error         *errorJSON      `json:"error,omitempty"`
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
	}
	if lastErr != nil {
		out.Error = errorObject(lastErr)
	}
	return env.emitJSON(out)
}
