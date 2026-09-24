package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxtap/v3"
	"github.com/spf13/cobra"
)

func newFormatsCmd() *cobra.Command {
	var noFallback bool
	cmd := &cobra.Command{
		Use:   "formats <url>",
		Short: "List the candidate audio formats for a video",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			noteUseBothWebSources(env)
			var ropts []waxtap.ReadOption
			if noFallback {
				ropts = append(ropts, waxtap.WithNoFallback())
			}
			info, err := env.client.InfoResult(cmd.Context(), args[0], waxtap.InfoBasic, ropts...)
			if err != nil {
				return err
			}
			noteFormatsSource(env, info)
			noteDroppedPlaylist(env, args[0], "enumerate it with `download <url> --list`")
			video := info.Video
			formats := audioFormats(video.Formats)
			if env.jsonMode() {
				out := make([]formatJSON, len(formats))
				for i, f := range formats {
					out[i] = formatToJSON(f)
				}
				return env.emitJSON(struct {
					SchemaVersion int    `json:"schemaVersion"`
					VideoID       string `json:"videoId"`
					Title         string `json:"title"`
					// Client, ViaWatchPage, and SubstitutedFrom describe where
					// the list came from, exactly as info reports them: a
					// consumer reading formats has the same question.
					Client          string       `json:"client,omitempty"`
					ViaWatchPage    bool         `json:"viaWatchPage,omitempty"`
					SubstitutedFrom string       `json:"substitutedFrom,omitempty"`
					Formats         []formatJSON `json:"formats"`
					Notes           []noteJSON   `json:"notes,omitempty"`
				}{schemaVersion, video.ID, video.Title, info.Client, info.ViaWatchPage, info.SubstitutedFrom, out, env.notesJSON()})
			}
			if len(formats) == 0 {
				env.printf("no audio formats found\n")
				return nil
			}
			return renderFormatsTable(env, formats)
		},
	}
	cmd.Flags().BoolVar(&noFallback, "no-fallback", false, "disable the watch-page extraction fallback")
	bindConfigFlags(cmd.Flags())
	bindNetworkFlags(cmd.Flags())
	bindPlayerExtractionFlags(cmd.Flags())
	return cmd
}

// audioFormats keeps the audio candidates, falling back to all formats when none
// are explicitly labeled audio (some player responses omit the MIME prefix). The
// result is deduplicated for display.
func audioFormats(all []waxtap.Format) []waxtap.Format {
	var audio []waxtap.Format
	for _, f := range all {
		if f.IsAudio() {
			audio = append(audio, f)
		}
	}
	if len(audio) == 0 {
		return dedupFormats(all)
	}
	return dedupFormats(audio)
}

// formatDedupKey identifies a display row. Distinct audio tracks and DRC variants
// keep separate rows even when they share an itag.
type formatDedupKey struct {
	itag     int
	track    string
	drc      waxtap.Tri
	original waxtap.Tri
}

func dedupKey(f waxtap.Format) formatDedupKey {
	return formatDedupKey{itag: f.Itag, track: trackID(f), drc: f.IsDRC, original: f.IsOriginal}
}

// trackID is the audio track a format names, "" on a single-track video. It is
// the row identity for the table and the audioTrackId key in --json; Language
// is only the tag part of it.
func trackID(f waxtap.Format) string {
	if f.AudioTrack == nil {
		return ""
	}
	return f.AudioTrack.ID
}

// dedupFormats removes repeated display rows while retaining distinct audio
// tracks, DRC variants, and original-track verdicts. The first occurrence wins
// to preserve source order.
// Stream selection continues to use the full format list.
func dedupFormats(formats []waxtap.Format) []waxtap.Format {
	seen := make(map[formatDedupKey]bool, len(formats))
	out := make([]waxtap.Format, 0, len(formats))
	for _, f := range formats {
		k := dedupKey(f)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}

// renderFormatsTable writes an aligned table of formats to stdout.
func renderFormatsTable(env *appEnv, formats []waxtap.Format) error {
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ITAG\tCODEC\tEXT\tKBPS\tTIER\tHZ\tCH\tTRACK\tORIG\tDRC\tSIZE")
	for _, f := range formats {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			f.Itag,
			dash(f.Codec),
			dash(f.Extension),
			f.EffectiveBitrate()/1000,
			f.AudioQuality.String(),
			intOrDash(f.SampleRate),
			intOrDash(f.Channels),
			dash(trackID(f)),
			triOrDash(f.IsOriginal),
			triOrDash(f.IsDRC),
			sizeOrDash(f.ContentLength),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// An itag may appear once for each DRC variant.
	if hasDRCVariant(formats) {
		env.printf("\nDRC=yes marks the dynamic-range-compressed variant; the same itag may also appear with DRC=no.\n")
		env.printf("Best-audio selection and --itag both prefer the original track and the full-range (DRC=no) variant when present.\n")
	}
	return nil
}

// hasDRCVariant reports whether the format list includes a DRC variant.
func hasDRCVariant(formats []waxtap.Format) bool {
	for _, f := range formats {
		if f.IsDRC == waxtap.Yes {
			return true
		}
	}
	return false
}

// formatJSON is the --json view for YouTube formats, using explicit CLI field
// names. Numeric fields stay present even when zero because YouTube uses zero for
// unknown values in some streams; durationSeconds is the exception, since zero
// there means unknown on every stream. itag is omitted only when no YouTube
// format is behind the source, and language and audioTrackId only on a
// single-track video. Local-file results use localFormatJSON to avoid
// network-only fields that would always be zero.
type formatJSON struct {
	Itag            int     `json:"itag,omitempty"`
	Codec           string  `json:"codec"`
	MIMEType        string  `json:"mimeType"`
	Extension       string  `json:"extension"`
	Bitrate         int     `json:"bitrate"`
	AverageBitrate  int     `json:"averageBitrate"`
	SampleRate      int     `json:"sampleRate"`
	Channels        int     `json:"channels"`
	AudioQuality    string  `json:"audioQuality"`
	Language        string  `json:"language,omitempty"`
	AudioTrackID    string  `json:"audioTrackId,omitempty"`
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
		AudioTrackID:    trackID(f),
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

// triOrDash renders an unknown tri-state value as a dash, consistent with the
// other optional table columns.
func triOrDash(t waxtap.Tri) string {
	if t == waxtap.Unknown {
		return "-"
	}
	return t.String()
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

// noteFormatsSource reports a listing that came from the watch-page fallback.
// One note for the whole fact, on every client rather than only a forced WEB
// one: the watch page needs no PO token, which is what a reader has to know,
// and a client substitution is a detail of that rather than a note of its own.
func noteFormatsSource(env *appEnv, info *waxtap.InfoResult) {
	if !info.ViaWatchPage {
		return
	}
	if info.SubstitutedFrom != "" {
		env.note(noteWatchPageFormats, "requested %s; listing %s formats from the watch-page fallback (no PO token)", info.SubstitutedFrom, info.Client)
		return
	}
	env.note(noteWatchPageFormats, "listing %s formats from the watch-page fallback (no PO token)", info.Client)
}
