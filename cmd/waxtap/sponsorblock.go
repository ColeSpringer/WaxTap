package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxtap/v2/sponsorblock"
	"github.com/colespringer/waxtap/v2/youtube"
	"github.com/spf13/cobra"
)

func newSponsorBlockCmd() *cobra.Command {
	var categories string
	cmd := &cobra.Command{
		Use:     "sponsorblock <url>",
		Aliases: []string{"sb"},
		Short:   "Preview SponsorBlock segments for a video (no download)",
		Args:    sponsorblockArgs(cobra.ExactArgs(1), false),
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
			// Route through the facade so the preview honors the configured proxy,
			// per-host limiter, SponsorBlock base URL, and fetch timeout.
			segs, err := env.client.SponsorBlockSegments(cmd.Context(), args[0], cats)
			if err != nil {
				return err
			}

			if env.jsonMode() {
				id, _ := youtube.ExtractVideoID(args[0]) // already validated by the fetch above
				return emitSponsorBlockJSON(env, id, segs)
			}
			renderSponsorBlockHuman(env, segs)
			return nil
		},
	}
	bindSponsorBlockFlag(cmd.Flags(), &categories, "categories to preview (comma-separated; bare flag selects music_offtopic)")
	bindConfigFlags(cmd.Flags())
	bindNetworkFlags(cmd.Flags())
	return cmd
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

func renderSponsorBlockHuman(env *appEnv, segs []sponsorblock.Segment) {
	if len(segs) == 0 {
		env.printf("no SponsorBlock segments\n")
		return
	}
	var total time.Duration
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tSTART\tEND\tLEN\tLOCKED\tVOTES")
	for _, s := range segs {
		total += s.End - s.Start
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%d\n",
			s.Category, humanDuration(s.Start), humanDuration(s.End), humanDuration(s.End-s.Start), s.Locked, s.Votes)
	}
	tw.Flush()
	env.printf("\n%d segment(s), %s would be removed\n", len(segs), humanDuration(total))
}

func emitSponsorBlockJSON(env *appEnv, videoID string, segs []sponsorblock.Segment) error {
	type segJSON struct {
		Category     string  `json:"category"`
		ActionType   string  `json:"actionType"`
		StartSeconds float64 `json:"startSeconds"`
		EndSeconds   float64 `json:"endSeconds"`
		UUID         string  `json:"uuid"`
		Locked       bool    `json:"locked"`
		Votes        int     `json:"votes"`
	}
	out := make([]segJSON, len(segs))
	var total time.Duration
	for i, s := range segs {
		total += s.End - s.Start
		out[i] = segJSON{
			Category:     string(s.Category),
			ActionType:   s.ActionType,
			StartSeconds: s.Start.Seconds(),
			EndSeconds:   s.End.Seconds(),
			UUID:         s.UUID,
			Locked:       s.Locked,
			Votes:        s.Votes,
		}
	}
	return env.emitJSON(struct {
		SchemaVersion  int       `json:"schemaVersion"`
		VideoID        string    `json:"videoId"`
		Segments       []segJSON `json:"segments"`
		RemovedSeconds float64   `json:"removedSeconds"`
	}{schemaVersion, videoID, out, total.Seconds()})
}
