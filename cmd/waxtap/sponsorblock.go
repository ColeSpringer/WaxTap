package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/sponsorblock"
	"github.com/colespringer/waxtap/v3/youtube"
	"github.com/spf13/cobra"
)

func newSponsorBlockCmd() *cobra.Command {
	var categories string
	cmd := &cobra.Command{
		Use:     "sponsorblock <url>",
		Aliases: []string{"sb"},
		Short:   "Preview SponsorBlock segments for a video (no download)",
		Long: "List the SponsorBlock segments a download would remove, without downloading.\n\n" +
			"The preview also fetches the video's own length, so a segment the database\n" +
			"holds past the end of the video is marked PAST END and left out of the\n" +
			"removal total. A length it could not fetch is reported as the\n" +
			"length-unchecked note, and the segments are taken at their word.",
		Args: sponsorblockArgs(cobra.ExactArgs(1), false),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			if err := rejectEmptySponsorBlock(cmd, categories); err != nil {
				return err
			}
			cats, err := parseCategories(categories)
			if err != nil {
				return err
			}
			return runSponsorBlockPreview(cmd.Context(), env, args[0], cats)
		},
	}
	bindSponsorBlockFlag(cmd.Flags(), &categories, "categories to preview (comma-separated; bare flag selects music_offtopic)")
	bindConfigFlags(cmd.Flags())
	bindNetworkFlags(cmd.Flags())
	bindSponsorBlockURLFlag(cmd.Flags())
	// The preview extracts once for the video's length, so it takes the
	// extraction flags like every other command that makes a player request.
	bindPlayerExtractionFlags(cmd.Flags())
	return cmd
}

// runSponsorBlockPreview fetches the segments and the video's length and
// renders the preview. It is split out so a test can hand it an appEnv.
func runSponsorBlockPreview(ctx context.Context, env *appEnv, target string, cats []sponsorblock.Category) error {
	// Route through the facade so the preview honors the configured proxy,
	// per-host limiter, SponsorBlock base URL, and fetch timeout.
	segs, err := env.client.SponsorBlockSegments(ctx, target, cats)
	if err != nil {
		return err
	}

	// SponsorBlock's database holds whatever contributors submitted, including
	// segments past the end of the video. Without the length there is no way
	// to tell those from real ones, and they inflate the removal total by
	// audio that does not exist.
	total := time.Duration(0)
	// Only when there is something to check against it. The length costs a
	// full player extraction, and a video with no segments has nothing for it
	// to place.
	if len(segs) == 0 {
		return renderSponsorBlockPreview(env, target, segs, total)
	}
	if v, ierr := env.client.Info(ctx, target, waxtap.InfoBasic); ierr != nil {
		// A cancellation is the caller stopping the run, not a length WaxTap
		// could not fetch: degrading it to a note would render the preview and
		// exit 0 on a Ctrl-C.
		if ctx.Err() != nil || errors.Is(ierr, context.Canceled) {
			return ierr
		}
		env.note(noteLengthUnchecked, "could not fetch the video's length (%v); segments were not checked against it", ierr)
	} else if v.Duration <= 0 {
		env.note(noteLengthUnchecked, "the player reported no length; segments were not checked against it")
	} else {
		total = v.Duration
	}

	return renderSponsorBlockPreview(env, target, segs, total)
}

// renderSponsorBlockPreview writes the preview in whichever form was asked for.
func renderSponsorBlockPreview(env *appEnv, target string, segs []sponsorblock.Segment, length time.Duration) error {
	if env.jsonMode() {
		id, _ := youtube.ExtractVideoID(target) // already validated by the fetch above
		return emitSponsorBlockJSON(env, id, segs, length)
	}
	return renderSponsorBlockHuman(env, segs, length)
}

// removedBy is the audio a segment would really remove: the part of it inside
// the video. A segment starting at or past the end removes nothing, and one
// that overruns the end removes only up to it. A zero total means the length
// is unknown, so the segment is taken at its word.
func removedBy(s sponsorblock.Segment, total time.Duration) time.Duration {
	if total <= 0 {
		return s.End - s.Start
	}
	if s.Start >= total {
		return 0
	}
	return min(s.End, total) - s.Start
}

// pastEnd reports a segment that starts at or after the video's end, which
// removes nothing at all. A known length is required to say so.
func pastEnd(s sponsorblock.Segment, total time.Duration) bool {
	return total > 0 && s.Start >= total
}

// rejectEmptySponsorBlock rejects --sponsorblock values with no categories after
// comma splitting and trimming, such as `--sponsorblock=` or `--sponsorblock=, ,`.
// A bare --sponsorblock still uses the flag's NoOptDefVal, and an unset flag is
// ignored.
func rejectEmptySponsorBlock(cmd *cobra.Command, value string) error {
	if !cmd.Flags().Changed("sponsorblock") {
		return nil
	}
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) != "" {
			return nil
		}
	}
	return usagef("--sponsorblock needs at least one category; use a bare --sponsorblock (no =) for music_offtopic")
}

// parseCategories parses a comma-separated category list, validating each. An
// empty list falls back to sponsorblock.DefaultCategories.
func parseCategories(csv string) ([]sponsorblock.Category, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return sponsorblock.DefaultCategories, nil
	}
	var cats []sponsorblock.Category
	for _, part := range strings.Split(csv, ",") {
		c := sponsorblock.Category(strings.TrimSpace(part))
		if c == "" {
			continue
		}
		if !c.Valid() {
			return nil, usagef("unknown SponsorBlock category %q", string(c))
		}
		cats = append(cats, c)
	}
	if len(cats) == 0 {
		return sponsorblock.DefaultCategories, nil
	}
	return cats, nil
}

func renderSponsorBlockHuman(env *appEnv, segs []sponsorblock.Segment, length time.Duration) error {
	if len(segs) == 0 {
		env.printf("no SponsorBlock segments\n")
		return nil
	}
	var removed time.Duration
	past := 0
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tSTART\tEND\tLEN\tLOCKED\tVOTES\tPAST END")
	for _, s := range segs {
		removed += removedBy(s, length)
		mark := ""
		if pastEnd(s, length) {
			mark, past = "yes", past+1
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%d\t%s\n",
			s.Category, humanDuration(s.Start), humanDuration(s.End), humanDuration(s.End-s.Start), s.Locked, s.Votes, mark)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	line := fmt.Sprintf("\n%d segment(s), %s would be removed", len(segs), humanDuration(removed))
	if past > 0 {
		line += fmt.Sprintf("; %s past the end", countOf(past, "segment"))
	}
	env.printf("%s\n", line)
	return nil
}

func emitSponsorBlockJSON(env *appEnv, videoID string, segs []sponsorblock.Segment, length time.Duration) error {
	type segJSON struct {
		Category     string  `json:"category"`
		ActionType   string  `json:"actionType"`
		StartSeconds float64 `json:"startSeconds"`
		EndSeconds   float64 `json:"endSeconds"`
		UUID         string  `json:"uuid"`
		Locked       bool    `json:"locked"`
		Votes        int     `json:"votes"`
		// PastEnd marks a segment starting at or after the video's end, which
		// removes nothing. Omitted when the length is unknown.
		PastEnd bool `json:"pastEnd,omitempty"`
	}
	out := make([]segJSON, len(segs))
	var removed time.Duration
	for i, s := range segs {
		removed += removedBy(s, length)
		out[i] = segJSON{
			Category:     string(s.Category),
			ActionType:   s.ActionType,
			StartSeconds: s.Start.Seconds(),
			EndSeconds:   s.End.Seconds(),
			UUID:         s.UUID,
			Locked:       s.Locked,
			Votes:        s.Votes,
			PastEnd:      pastEnd(s, length),
		}
	}
	return env.emitJSON(struct {
		SchemaVersion int       `json:"schemaVersion"`
		VideoID       string    `json:"videoId"`
		Segments      []segJSON `json:"segments"`
		// DurationSeconds is the video's own length, omitted when the preview
		// could not fetch it (the length-unchecked note says so).
		DurationSeconds float64 `json:"durationSeconds,omitempty"`
		// RemovedSeconds counts only the part of each segment inside the
		// video, so a segment past the end contributes nothing.
		RemovedSeconds float64    `json:"removedSeconds"`
		Notes          []noteJSON `json:"notes,omitempty"`
	}{schemaVersion, videoID, out, length.Seconds(), removed.Seconds(), env.notesJSON()})
}
