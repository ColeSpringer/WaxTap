package main

import (
	"github.com/colespringer/waxtap/v2"
	"github.com/spf13/cobra"
)

func newInfoCmd() *cobra.Command {
	var (
		showURLs   bool
		probe      bool
		full       bool
		channels   string
		noFallback bool
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
			noteUseBothWebSources(env)
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
			// Resolve/probe the same row the human and JSON output display as "Best
			// audio", so --probe refines the displayed row, not a surround track.
			ropts := []waxtap.ReadOption{waxtap.WithChannels(layout)}
			if noFallback {
				ropts = append(ropts, waxtap.WithNoFallback())
			}
			if full {
				ropts = append(ropts, waxtap.WithFullMetadata())
			}
			info, err := env.client.InfoResult(cmd.Context(), args[0], depth, ropts...)
			if err != nil {
				return err
			}
			emitWatchPageBreadcrumb(env, info)
			noteDroppedPlaylist(env, args[0], "enumerate it with `download <url> --list`")
			video := info.Video

			var resolved *waxtap.ResolvedStream
			if showURLs {
				rs, rerr := env.client.Resolve(cmd.Context(), args[0], sel, ropts...)
				if rerr != nil {
					return rerr
				}
				resolved = &rs
			}

			// Prefer the row InfoResult actually resolved/probed: applyProbe mutates
			// that row in place, so re-selecting on the mutated slice could land on a
			// different near-tie row than the one shown as (probed).
			var bestErr error
			bestIdx := info.BestIndex
			if info.BestIndex < 0 {
				bestIdx, bestErr = sel.Select(video.Formats, waxtap.MinimizeLoss(), waxtap.Target{})
			}

			if env.jsonMode() {
				return emitInfoJSON(env, info, bestIdx, bestErr, resolved)
			}
			renderInfoHuman(env, info, bestIdx, bestErr, resolved, showURLs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&showURLs, "show-url", false, "resolve and print the signed best-audio stream URL (sensitive, expires)")
	cmd.Flags().BoolVar(&probe, "probe", false, "ffprobe the selected stream for authoritative rate/channels/bitrate (requires ffmpeg)")
	cmd.Flags().BoolVar(&full, "full", false, "fetch full metadata (publish date, chapters) via a token-free watch-page pass")
	cmd.Flags().StringVar(&channels, "channels", "stereo", "channel layout to prefer for 'Best audio': mono|stereo|surround|any")
	cmd.Flags().BoolVar(&noFallback, "no-fallback", false, "disable the watch-page extraction fallback")
	bindConfigFlags(cmd.Flags())
	bindNetworkFlags(cmd.Flags())
	bindPlayerExtractionFlags(cmd.Flags())
	return cmd
}

func renderInfoHuman(env *appEnv, info *waxtap.InfoResult, bestIdx int, bestErr error, rs *waxtap.ResolvedStream, showURLs bool) {
	v := info.Video
	env.printf("Title:     %s\n", v.Title)
	env.printf("Author:    %s\n", v.Author)
	env.printf("Video ID:  %s\n", v.ID)
	if info.Client != "" {
		env.printf("Client:    %s\n", info.Client)
	}
	if info.SubstitutedFrom != "" {
		env.printf("  (requested %s; fell back to %s)\n", info.SubstitutedFrom, info.Client)
	}
	env.printf("Duration:  %s\n", humanDuration(v.Duration))
	if !v.PublishDate.IsZero() {
		env.printf("Published: %s\n", v.PublishDate.Format("2006-01-02"))
	}
	if v.LiveStatus != waxtap.LiveNone {
		env.printf("Live:      %s\n", v.LiveStatus)
	}
	env.printf("Formats:   %d audio candidate(s)\n", len(audioFormats(v.Formats)))
	if len(v.Chapters) > 0 {
		env.printf("Chapters:  %d\n", len(v.Chapters))
		renderChapters(env, v.Chapters)
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
		if info.Probed {
			// The row's numbers came from ffprobe of the resolved stream, not the
			// player manifest.
			env.printf("  (probed)")
		}
		env.printf("\n")
		if info.Probed && f.Duration > 0 {
			env.printf("  length:  %s\n", humanDuration(f.Duration))
		}
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

func emitInfoJSON(env *appEnv, info *waxtap.InfoResult, bestIdx int, bestErr error, rs *waxtap.ResolvedStream) error {
	v := info.Video
	// Match the human display without changing the selection indexed by bestIdx.
	deduped := dedupFormats(v.Formats)
	formats := make([]formatJSON, len(deduped))
	for i, f := range deduped {
		formats[i] = formatToJSON(f)
	}
	// dedupFormats keeps the first row per {itag,track,drc}. When the probed best row
	// is a later duplicate, overlay its authoritative numbers onto the kept row so
	// the formats[] entry agrees with the human "Best audio" line. Finalize formats
	// before building out so the slice is not mutated afterward.
	if bestErr == nil && info.Probed {
		bestKey := dedupKey(v.Formats[bestIdx])
		for i := range deduped {
			if dedupKey(deduped[i]) == bestKey {
				formats[i] = formatToJSON(v.Formats[bestIdx])
				break
			}
		}
	}
	out := struct {
		SchemaVersion   int           `json:"schemaVersion"`
		VideoID         string        `json:"videoId"`
		Title           string        `json:"title"`
		Author          string        `json:"author"`
		Client          string        `json:"client,omitempty"`
		SubstitutedFrom string        `json:"substitutedFrom,omitempty"`
		ChannelID       string        `json:"channelId,omitempty"`
		DurationSecs    float64       `json:"durationSeconds"`
		PublishDate     string        `json:"publishDate,omitempty"`
		IsLive          bool          `json:"isLive"`
		IsUpcoming      bool          `json:"isUpcoming"`
		LiveStatus      string        `json:"liveStatus,omitempty"`
		Availability    string        `json:"availability,omitempty"`
		ChapterCount    int           `json:"chapterCount"`
		Chapters        []chapterJSON `json:"chapters,omitempty"`
		Formats         []formatJSON  `json:"formats"`
		BestAudioItag   *int          `json:"bestAudioItag,omitempty"`
		Resolved        *resolvedJSON `json:"resolved,omitempty"`
	}{
		SchemaVersion:   schemaVersion,
		VideoID:         v.ID,
		Title:           v.Title,
		Author:          v.Author,
		Client:          info.Client,
		SubstitutedFrom: info.SubstitutedFrom,
		ChannelID:       v.ChannelID,
		DurationSecs:    v.Duration.Seconds(),
		// Derive the long-standing booleans from LiveStatus. A returned Video is
		// never live or upcoming (those are error sentinels), so both stay false and
		// the JSON shape is unchanged.
		IsLive:     v.LiveStatus == waxtap.LiveNow,
		IsUpcoming: v.LiveStatus == waxtap.LiveUpcoming,
		// liveStatus/availability are additive and omitempty: they surface a was-live
		// VOD or an unlisted video the booleans cannot express, while a normal video
		// (with or without --full) stays byte-identical. See the helpers below.
		LiveStatus:   infoLiveStatus(v.LiveStatus),
		Availability: infoAvailability(v.Availability),
		// chapterCount is unchanged; the chapters array is additive and only present
		// when a watch-page pass (info --full) populated it, so the schema stays 1.
		ChapterCount: len(v.Chapters),
		Chapters:     chaptersToJSON(v.Chapters),
		Formats:      formats,
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

// infoLiveStatus renders a Video's live status for --json, emitting only the new
// signal. A returned Video is only ever LiveNone or LiveWasLive (live/upcoming are
// error sentinels), so LiveNone maps to "" and normal-video output is unchanged;
// a completed livestream emits "was_live".
func infoLiveStatus(s waxtap.LiveStatus) string {
	if s == waxtap.LiveNone {
		return ""
	}
	return s.String()
}

// infoAvailability renders a Video's availability for --json, emitting only the new
// signal, "unlisted". AvailabilityUnknown (no watch-page pass) and AvailabilityPublic
// (which --full resolves for a normal video) both map to "", so existing output stays
// byte-identical.
func infoAvailability(a waxtap.Availability) string {
	if a == waxtap.AvailabilityUnlisted {
		return a.String()
	}
	return ""
}

type resolvedJSON struct {
	URL           string `json:"url"`
	IsSABR        bool   `json:"isSabr,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
	ContentLength int64  `json:"contentLength"`
}

// chapterJSON is one chapter in the info --json output. EndSeconds is omitted for
// an open-ended last chapter (unknown duration) rather than emitting an end before
// the start.
type chapterJSON struct {
	StartSeconds float64  `json:"startSeconds"`
	EndSeconds   *float64 `json:"endSeconds,omitempty"`
	Title        string   `json:"title"`
}

// chaptersToJSON maps chapters to their JSON form. It returns nil for none, so the
// omitempty chapters field is absent unless a watch-page pass populated it.
func chaptersToJSON(chapters []waxtap.Chapter) []chapterJSON {
	if len(chapters) == 0 {
		return nil
	}
	out := make([]chapterJSON, len(chapters))
	for i, c := range chapters {
		out[i] = chapterJSON{StartSeconds: c.Start.Seconds(), Title: c.Title}
		if c.End > c.Start {
			end := c.End.Seconds()
			out[i].EndSeconds = &end
		}
	}
	return out
}

// renderChapters prints the chapter list under the count line. Each line shows the
// time range and title; an open-ended last chapter shows only its start.
func renderChapters(env *appEnv, chapters []waxtap.Chapter) {
	for _, c := range chapters {
		if c.End > c.Start {
			env.printf("  %s-%s  %s\n", humanDuration(c.Start), humanDuration(c.End), c.Title)
		} else {
			env.printf("  %s  %s\n", humanDuration(c.Start), c.Title)
		}
	}
}
