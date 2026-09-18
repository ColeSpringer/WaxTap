package main

import (
	"github.com/colespringer/waxtap/v3"
	"github.com/spf13/cobra"
)

func newInfoCmd() *cobra.Command {
	var (
		showURLs     bool
		probe        bool
		full         bool
		channels     string
		itag         int
		codec        string
		sourcePolicy string
		noFallback   bool
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
			// Build the selection from the same helpers download uses, so info
			// answers "what would this request give me?" instead of only ever
			// reporting the default pick. audioSelector also enforces the
			// --itag/--codec exclusivity, so both commands reject the same input.
			layout, err := parseChannels(resolveChannelsFlag(cmd, env.cfg, channels))
			if err != nil {
				return err
			}
			if err := validateItag(cmd, itag); err != nil {
				return err
			}
			sel, err := audioSelector(itag, codec, layout)
			if err != nil {
				return err
			}
			policy, err := parseSourcePolicy(sourcePolicy)
			if err != nil {
				return err
			}

			depth := waxtap.InfoBasic
			if probe {
				depth = waxtap.InfoProbe
			}
			// Resolve/probe the same row the human and JSON output display as "Best
			// audio", so --probe refines the displayed row, not a surround track.
			// The selector already carries --channels (audioSelector applies it to
			// every selector kind that honors a layout), so WithChannels alongside
			// it would say nothing new.
			ropts := []waxtap.ReadOption{waxtap.WithSelector(sel), waxtap.WithSourcePolicy(policy)}
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
			noteDroppedPlaylist(env, args[0], "enumerate it with `download <url> --list`")
			noteProbeSkippedIfUnread(env, probe, info)
			for _, w := range info.Warnings {
				env.info("warning: [%s] %s\n", w.Code, w.Detail)
			}
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
				bestIdx, bestErr = sel.Select(video.Formats, policy, waxtap.Target{})
			}

			// Same rule download uses for channelsExplicit: a configured default is
			// as deliberate as the flag, and the built-in stereo default stays quiet.
			explicit := cmd.Flags().Changed("channels") || env.cfg.channels != ""
			noteInfoChannelLayout(env, layout, explicit, itag, video.Formats, bestIdx, bestErr)
			// A selector that matched nothing would otherwise print full metadata
			// with the "Best audio" line simply absent, which reads as a video with
			// no audio rather than as a request that named a row this video does
			// not carry. Only for an explicit selector: the default one cannot miss.
			if bestErr != nil && (itag > 0 || codec != "") {
				env.note(noteSelectionUnmatched, "no audio format matched the requested selection: %v", bestErr)
			}

			if env.jsonMode() {
				return emitInfoJSON(env, info, bestIdx, bestErr, resolved)
			}
			renderInfoHuman(env, info, bestIdx, bestErr, resolved, showURLs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&showURLs, "show-url", false, "resolve and print the signed URL of the selected stream (sensitive, expires)")
	cmd.Flags().BoolVar(&probe, "probe", false, "probe the selected stream for authoritative rate/channels/duration")
	cmd.Flags().BoolVar(&full, "full", false, "fetch full metadata (publish date, chapters) via a token-free watch-page pass")
	cmd.Flags().StringVar(&channels, "channels", "stereo", "channel layout to prefer for 'Best audio': mono|stereo|surround|any")
	cmd.Flags().IntVar(&itag, "itag", 0, "report an exact itag instead of the best audio")
	cmd.Flags().StringVar(&codec, "codec", "", "report the best source matching a codec (hard filter)")
	cmd.Flags().StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source policy: minimize-loss|best-native|prefer:<codec> (info names no transcode target, so only prefer:<codec> shifts the pick)")
	cmd.Flags().BoolVar(&noFallback, "no-fallback", false, "disable the watch-page extraction fallback")
	bindConfigFlags(cmd.Flags())
	bindNetworkFlags(cmd.Flags())
	bindPlayerExtractionFlags(cmd.Flags())
	return cmd
}

// noteInfoChannelLayout reports when the row shown as "Best audio" does not
// satisfy an explicitly requested layout. Selection prefers the layout but falls
// back to the best available stream, so the mismatch is a note rather than an
// error, the same as download's warnChannelLayout.
//
// An exact --itag gets the reason instead of the bare mismatch, as it does in
// download: format.Select's itag branch ignores the layout, so --channels never
// entered the selection at all, and the plain note would read as a preference
// that lost a ranking it never ran in.
func noteInfoChannelLayout(env *appEnv, layout waxtap.ChannelLayout, explicit bool, itag int, formats []waxtap.Format, bestIdx int, bestErr error) {
	if !explicit || layout == waxtap.LayoutAny || bestErr != nil {
		return
	}
	if bestIdx < 0 || bestIdx >= len(formats) {
		return
	}
	delivered := formats[bestIdx].Channels
	if delivered <= 0 || layout.Matches(delivered) {
		return
	}
	if itag > 0 {
		env.note(noteChannelsIgnored, "--itag names an exact encoding, so --channels did not affect selection; requested %s, best audio is %s",
			layout, channelCountLabel(delivered))
		return
	}
	env.note(noteChannelsUnavailable, "requested %s; best audio is %s", layout, channelCountLabel(delivered))
}

func renderInfoHuman(env *appEnv, info *waxtap.InfoResult, bestIdx int, bestErr error, rs *waxtap.ResolvedStream, showURLs bool) {
	v := info.Video
	env.printf("Title:     %s\n", v.Title)
	env.printf("Author:    %s\n", v.Author)
	env.printf("Video ID:  %s\n", v.ID)
	if info.Client != "" {
		env.printf("Client:    %s%s\n", info.Client, watchPageSuffix(info.ViaWatchPage))
	}
	if info.SubstitutedFrom != "" {
		env.printf("  (requested %s; fell back to %s)\n", info.SubstitutedFrom, info.Client)
	}
	env.printf("Duration:  %s\n", durationOrDash(v.Duration))
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
			// The row's numbers came from a probe of the resolved stream, not the
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

// noteProbeSkippedIfUnread records that --probe read nothing. A SABR-only pick
// has no direct URL to stage, so the row still carries the manifest's numbers,
// and silence there reads as a probe that agreed with the manifest.
func noteProbeSkippedIfUnread(env *appEnv, asked bool, info *waxtap.InfoResult) {
	if asked && !info.Probed && info.BestIndex >= 0 {
		env.note(noteProbeSkipped, "the selected stream is SABR-only (no direct URL), so --probe read nothing; the row's numbers are the manifest's")
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
		ViaWatchPage    bool          `json:"viaWatchPage,omitempty"`
		SubstitutedFrom string        `json:"substitutedFrom,omitempty"`
		ChannelID       string        `json:"channelId,omitempty"`
		DurationSecs    float64       `json:"durationSeconds,omitempty"` // absent when the source reported none
		PublishDate     string        `json:"publishDate,omitempty"`
		IsLive          bool          `json:"isLive"`
		IsUpcoming      bool          `json:"isUpcoming"`
		LiveStatus      string        `json:"liveStatus,omitempty"`
		Availability    string        `json:"availability,omitempty"`
		FullMetadata    bool          `json:"fullMetadata"`
		ChapterCount    *int          `json:"chapterCount,omitempty"`
		Chapters        []chapterJSON `json:"chapters,omitempty"`
		Formats         []formatJSON  `json:"formats"`
		BestAudioItag   *int          `json:"bestAudioItag,omitempty"`
		Resolved        *resolvedJSON `json:"resolved,omitempty"`
		Warnings        []warningJSON `json:"warnings,omitempty"`
		Notes           []noteJSON    `json:"notes,omitempty"`
	}{
		SchemaVersion:   schemaVersion,
		VideoID:         v.ID,
		Title:           v.Title,
		Author:          v.Author,
		Client:          info.Client,
		ViaWatchPage:    info.ViaWatchPage,
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
		// fullMetadata says whether the watch-page pass ran. It is the honest
		// signal behind the three keys that depend on it: chapterCount below, and
		// liveStatus/availability, which a consumer should not trust without it.
		FullMetadata: info.FullMetadata,
		Chapters:     chaptersToJSON(v.Chapters),
		Formats:      formats,
	}
	for _, w := range info.Warnings {
		out.Warnings = append(out.Warnings, warningJSON{Code: w.Code.String(), Detail: w.Detail})
	}
	// chapterCount is a pointer so it can be absent rather than 0. Without the
	// full pass no chapters were ever fetched, and reporting 0 asserted a video
	// has none when nothing had looked: a consumer building a chapter index read
	// that as a negative answer. With the pass, 0 is a real answer and is emitted.
	// bestAudioItag is the same pattern.
	if info.FullMetadata {
		n := len(v.Chapters)
		out.ChapterCount = &n
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
	out.Notes = env.notesJSON()
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
