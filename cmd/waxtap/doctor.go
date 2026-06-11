package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

// doctorCandidates are known public videos used for extraction checks. doctor
// tries them in order so one unavailable video does not decide the result.
var doctorCandidates = []string{
	"jNQXAC9IVRw", // "Me at the zoo", the first YouTube video
	"rFejpH_tAHM", // dotGo 2015, Rob Pike
	"BaW_jenozKc", // common extractor test video
}

// doctorByteProbe is how many bytes the cheap check reads to prove byte delivery.
const doctorByteProbe = 64 << 10

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
			if videoID != "" {
				candidates = []string{videoID}
			}

			ffmpegPath, _ := exec.LookPath("ffmpeg")
			rep := &doctorReport{FFmpegPath: ffmpegPath}

			var lastErr error
			for _, id := range candidates {
				env.info("checking %s…\n", id)
				if err := runDoctorCheck(cmd.Context(), env, id, full, rep); err != nil {
					lastErr = err
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
			} else {
				renderDoctorHuman(env, rep, lastErr)
			}
			return lastErr
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "download a complete track instead of a small byte range")
	cmd.Flags().StringVar(&videoID, "video-id", "", "check this specific video/URL instead of the built-in list")
	return cmd
}

type doctorReport struct {
	FFmpegPath string
	Healthy    bool
	VideoID    string
	Itag       int
	Bytes      int64
	Full       bool
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
		rep.Full = true
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
	if rep.FFmpegPath != "" {
		env.printf("ffmpeg:   found (%s)\n", rep.FFmpegPath)
	} else {
		env.printf("ffmpeg:   not found (processing commands unavailable)\n")
	}
	if rep.Healthy {
		mode := "range read"
		if rep.Full {
			mode = "full download"
		}
		env.printf("extract:  ok (%s)\n", rep.VideoID)
		env.printf("resolve:  ok (itag %d)\n", rep.Itag)
		env.printf("download: ok (%s, %s)\n", mode, humanBytes(rep.Bytes))
		env.printf("\nhealthy\n")
		return
	}
	env.printf("\nUNHEALTHY: %s\n", friendlyError(lastErr))
}

func emitDoctorJSON(env *appEnv, rep *doctorReport, lastErr error) error {
	out := struct {
		SchemaVersion int    `json:"schemaVersion"`
		Healthy       bool   `json:"healthy"`
		FFmpegFound   bool   `json:"ffmpegFound"`
		FFmpegPath    string `json:"ffmpegPath,omitempty"`
		VideoID       string `json:"videoId,omitempty"`
		Itag          int    `json:"itag,omitempty"`
		Bytes         int64  `json:"bytes,omitempty"`
		FullDownload  bool   `json:"fullDownload"`
		Error         string `json:"error,omitempty"`
	}{
		SchemaVersion: schemaVersion,
		Healthy:       rep.Healthy,
		FFmpegFound:   rep.FFmpegPath != "",
		FFmpegPath:    rep.FFmpegPath,
		VideoID:       rep.VideoID,
		Itag:          rep.Itag,
		Bytes:         rep.Bytes,
		FullDownload:  rep.Full,
	}
	if lastErr != nil {
		out.Error = lastErr.Error()
	}
	return env.emitJSON(out)
}
