package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// argsOutcome is the expected result of the wrapped positional-args validator.
type argsOutcome int

const (
	wantHint   argsOutcome = iota // the "=" misparse hint fires
	wantOK                        // delegates to base, which accepts
	wantReject                    // delegates to base, which rejects on arity (no hint)
)

// TestSponsorBlockArgsMisparse verifies that space-separated flag values receive
// a useful hint without changing normal positional-argument validation.
func TestSponsorBlockArgsMisparse(t *testing.T) {
	cases := []struct {
		name string
		cmd  func() *cobra.Command
		args []string
		want argsOutcome
	}{
		{"download space form", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock", "sponsor,intro"}, wantHint},
		{"download typo token", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock", "blah"}, wantHint},
		{"download equals form", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock=sponsor,intro"}, wantOK},
		{"download bare", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock"}, wantOK},
		{"download no flag", newDownloadCmd, []string{"dummyVideo0"}, wantOK},
		// ExactArgs(1) must still reject a surplus without --sponsorblock.
		{"download surplus no flag", newDownloadCmd, []string{"dummyVideo0", "extra"}, wantReject},
		// With -o set, a space-separated value is an impossible surplus.
		{"cut with -o set", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor", "-o", "out.webm"}, wantHint},
		{"cut trailing comma with -o", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor,", "-o", "out.webm"}, wantHint},
		// Without -o, a comma-separated category list occupies the output slot.
		{"cut comma form", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor,intro"}, wantHint},
		// A space-separated value plus an output positional exceeds the budget.
		{"cut comma cats plus output", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor,intro", "out.flac"}, wantHint},
		{"cut single cat plus output", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor", "out.flac"}, wantHint},
		// A lone token in the output slot may be a valid filename.
		{"cut single token output slot", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor"}, wantOK},
		{"cut non-category output", newCutCmd, []string{"in.flac", "--sponsorblock", "myclip.mp3"}, wantOK},
		// The "=" form remains valid even when the output is named like a category.
		{"cut equals default plus category output", newCutCmd, []string{"in.flac", "--sponsorblock=music_offtopic", "intro"}, wantOK},
		{"cut plain output no flag", newCutCmd, []string{"in.flac", "out.flac"}, wantOK},
		// RangeArgs(1,2) must still reject a third argument without --sponsorblock.
		{"cut surplus no flag", newCutCmd, []string{"in.flac", "out.flac", "extra"}, wantReject},
		{"sponsorblock preview space form", newSponsorBlockCmd, []string{"dummyVideo0", "--sponsorblock", "sponsor"}, wantHint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.cmd()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}
			err := cmd.ValidateArgs(cmd.Flags().Args())
			isHint := err != nil && isUsageError(err) && strings.Contains(err.Error(), "attached with '='")
			switch tc.want {
			case wantHint:
				if !isHint {
					t.Errorf("ValidateArgs(%v) = %v, want the '=' hint", tc.args, err)
				}
			case wantOK:
				if err != nil {
					t.Errorf("ValidateArgs(%v) = %v, want nil (base accepts)", tc.args, err)
				}
			case wantReject:
				// Delegation must still enforce arity: an error, but not our hint.
				if isHint {
					t.Errorf("ValidateArgs(%v) = %v, want a base arity rejection, not the '=' hint", tc.args, err)
				}
				if err == nil {
					t.Errorf("ValidateArgs(%v) = nil, want the base validator to reject the surplus", tc.args)
				}
			}
		})
	}
}
