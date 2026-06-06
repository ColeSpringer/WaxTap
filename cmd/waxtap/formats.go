package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

func newFormatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "formats <url>",
		Short: "List the candidate audio formats for a video",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			video, err := env.client.Info(cmd.Context(), args[0], waxtap.InfoBasic)
			if err != nil {
				return err
			}
			formats := audioFormats(video.Formats)
			if env.jsonMode() {
				out := make([]formatJSON, len(formats))
				for i, f := range formats {
					out[i] = formatToJSON(f)
				}
				return env.emitJSON(struct {
					SchemaVersion int          `json:"schemaVersion"`
					VideoID       string       `json:"videoId"`
					Title         string       `json:"title"`
					Formats       []formatJSON `json:"formats"`
				}{schemaVersion, video.ID, video.Title, out})
			}
			if len(formats) == 0 {
				env.printf("no audio formats found\n")
				return nil
			}
			renderFormatsTable(env, formats)
			return nil
		},
	}
}

// audioFormats keeps the audio candidates, falling back to all formats when none
// are explicitly labeled audio (some player responses omit the MIME prefix).
func audioFormats(all []waxtap.Format) []waxtap.Format {
	var audio []waxtap.Format
	for _, f := range all {
		if f.IsAudio() {
			audio = append(audio, f)
		}
	}
	if len(audio) == 0 {
		return all
	}
	return audio
}

// renderFormatsTable writes an aligned table of formats to stdout.
func renderFormatsTable(env *appEnv, formats []waxtap.Format) {
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ITAG\tCODEC\tEXT\tKBPS\tTIER\tHZ\tCH\tLANG\tORIG\tDRC\tSIZE")
	for _, f := range formats {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			f.Itag,
			dash(f.Codec),
			dash(f.Extension),
			f.EffectiveBitrate()/1000,
			f.AudioQuality.String(),
			intOrDash(f.SampleRate),
			intOrDash(f.Channels),
			dash(f.Language),
			f.IsOriginal.String(),
			f.IsDRC.String(),
			sizeOrDash(f.ContentLength),
		)
	}
	tw.Flush()
}

// formatJSON is the --json view of a format, using explicit CLI field names.
type formatJSON struct {
	Itag            int     `json:"itag"`
	Codec           string  `json:"codec"`
	MIMEType        string  `json:"mimeType"`
	Extension       string  `json:"extension"`
	Bitrate         int     `json:"bitrate"`
	AverageBitrate  int     `json:"averageBitrate"`
	SampleRate      int     `json:"sampleRate"`
	Channels        int     `json:"channels"`
	AudioQuality    string  `json:"audioQuality"`
	Language        string  `json:"language,omitempty"`
	IsOriginal      string  `json:"isOriginal"`
	IsDRC           string  `json:"isDrc"`
	ContentLength   int64   `json:"contentLength"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
}

func formatToJSON(f waxtap.Format) formatJSON {
	return formatJSON{
		Itag:            f.Itag,
		Codec:           f.Codec,
		MIMEType:        f.MIMEType,
		Extension:       f.Extension,
		Bitrate:         f.Bitrate,
		AverageBitrate:  f.AverageBitrate,
		SampleRate:      f.SampleRate,
		Channels:        f.Channels,
		AudioQuality:    f.AudioQuality.String(),
		Language:        f.Language,
		IsOriginal:      f.IsOriginal.String(),
		IsDRC:           f.IsDRC.String(),
		ContentLength:   f.ContentLength,
		DurationSeconds: f.Duration.Seconds(),
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func intOrDash(n int) string {
	if n <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

func sizeOrDash(n int64) string {
	if n <= 0 {
		return "-"
	}
	return humanBytes(n)
}
