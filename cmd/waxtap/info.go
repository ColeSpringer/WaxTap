package main

import (
	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

func newInfoCmd() *cobra.Command {
	var (
		showURLs bool
		probe    bool
		channels string
	)
	cmd := &cobra.Command{
		Use:   "info <url>",
		Short: "Show video metadata and audio formats (no download)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			// Use download's source preference so "Best audio" reports the format
			// a default download would select.
			layout, err := parseChannels(resolveChannelsFlag(cmd, env.cfg, channels))
			if err != nil {
				return err
			}
			sel := waxtap.BestAudio().WithChannels(layout)

			depth := waxtap.InfoBasic
			if probe {
				depth = waxtap.InfoProbe
			}
			video, err := env.client.Info(cmd.Context(), args[0], depth)
			if err != nil {
				return err
			}

			var resolved *waxtap.ResolvedStream
			if showURLs {
				rs, rerr := env.client.Resolve(cmd.Context(), args[0], sel)
				if rerr != nil {
					return rerr
				}
				resolved = &rs
			}

			bestIdx, bestErr := sel.Select(video.Formats, waxtap.MinimizeLoss(), waxtap.Target{})

			if env.jsonMode() {
				return emitInfoJSON(env, video, bestIdx, bestErr, resolved)
			}
			renderInfoHuman(env, video, bestIdx, bestErr, resolved, showURLs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&showURLs, "show-urls", false, "resolve and print the signed best-audio stream URL (sensitive, expires)")
	cmd.Flags().BoolVar(&probe, "probe", false, "ffprobe the selected stream for authoritative rate/channels/bitrate (requires ffmpeg)")
	cmd.Flags().StringVar(&channels, "channels", "stereo", "channel layout to prefer for 'Best audio': mono|stereo|surround|any")
	return cmd
}

func renderInfoHuman(env *appEnv, v *waxtap.Video, bestIdx int, bestErr error, rs *waxtap.ResolvedStream, showURLs bool) {
	env.printf("Title:     %s\n", v.Title)
	env.printf("Author:    %s\n", v.Author)
	env.printf("Video ID:  %s\n", v.ID)
	env.printf("Duration:  %s\n", humanDuration(v.Duration))
	if !v.PublishDate.IsZero() {
		env.printf("Published: %s\n", v.PublishDate.Format("2006-01-02"))
	}
	if v.IsLive || v.IsUpcoming {
		env.printf("Live:      %v (upcoming: %v)\n", v.IsLive, v.IsUpcoming)
	}
	env.printf("Formats:   %d audio candidate(s)\n", len(audioFormats(v.Formats)))
	if len(v.Chapters) > 0 {
		env.printf("Chapters:  %d\n", len(v.Chapters))
	}

	if bestErr == nil {
		f := v.Formats[bestIdx]
		env.printf("\nBest audio: itag %d  %s  %s  %d kbps", f.Itag, dash(f.Codec), dash(f.Extension), f.EffectiveBitrate()/1000)
		if f.SampleRate > 0 {
			env.printf("  %d Hz", f.SampleRate)
		}
		if f.Channels > 0 {
			env.printf("  %dch", f.Channels)
		}
		if f.IsOriginal == waxtap.Yes {
			env.printf("  (original)")
		}
		env.printf("\n")
		if f.ContentLength > 0 {
			env.printf("  size:    %s\n", humanBytes(f.ContentLength))
		}
	}
	if rs != nil {
		if showURLs {
			if rs.IsSABR {
				env.printf("  url:     SABR (no direct URL)\n")
			} else {
				env.printf("  url:     %s\n", rs.URL)
			}
		}
		if !rs.ExpiresAt.IsZero() {
			env.printf("  expires: %s\n", rs.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"))
		}
	}
}

func emitInfoJSON(env *appEnv, v *waxtap.Video, bestIdx int, bestErr error, rs *waxtap.ResolvedStream) error {
	// Match the human display without changing the selection indexed by bestIdx.
	deduped := dedupFormats(v.Formats)
	formats := make([]formatJSON, len(deduped))
	for i, f := range deduped {
		formats[i] = formatToJSON(f)
	}
	out := struct {
		SchemaVersion int           `json:"schemaVersion"`
		VideoID       string        `json:"videoId"`
		Title         string        `json:"title"`
		Author        string        `json:"author"`
		ChannelID     string        `json:"channelId,omitempty"`
		DurationSecs  float64       `json:"durationSeconds"`
		PublishDate   string        `json:"publishDate,omitempty"`
		IsLive        bool          `json:"isLive"`
		IsUpcoming    bool          `json:"isUpcoming"`
		Chapters      int           `json:"chapterCount"`
		Formats       []formatJSON  `json:"formats"`
		BestAudioItag *int          `json:"bestAudioItag,omitempty"`
		Resolved      *resolvedJSON `json:"resolved,omitempty"`
	}{
		SchemaVersion: schemaVersion,
		VideoID:       v.ID,
		Title:         v.Title,
		Author:        v.Author,
		ChannelID:     v.ChannelID,
		DurationSecs:  v.Duration.Seconds(),
		IsLive:        v.IsLive,
		IsUpcoming:    v.IsUpcoming,
		Chapters:      len(v.Chapters),
		Formats:       formats,
	}
	if !v.PublishDate.IsZero() {
		out.PublishDate = v.PublishDate.Format("2006-01-02")
	}
	if bestErr == nil {
		itag := v.Formats[bestIdx].Itag
		out.BestAudioItag = &itag
	}
	if rs != nil {
		out.Resolved = &resolvedJSON{
			URL:           rs.URL,
			IsSABR:        rs.IsSABR,
			ContentLength: rs.ContentLength,
		}
		if !rs.ExpiresAt.IsZero() {
			out.Resolved.ExpiresAt = rs.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
		}
	}
	return env.emitJSON(out)
}

type resolvedJSON struct {
	URL           string `json:"url"`
	IsSABR        bool   `json:"isSabr,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
	ContentLength int64  `json:"contentLength"`
}
