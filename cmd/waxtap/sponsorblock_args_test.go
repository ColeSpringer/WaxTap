package main

import (
	"io"
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
		// With the flag and value before the target, the category list lands at
		// args[0]. The scan must catch it in any positional slot.
		{"download natural order", newDownloadCmd, []string{"--sponsorblock", "sponsor,intro", "dummyVideo0"}, wantHint},
		{"download single category space form", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock", "sponsor"}, wantHint},
		// A bare --sponsorblock plus a surplus non-target arg is most likely a
		// space-separated value (a misspelled category or a stray token), so it gets
		// the "=" hint. Commands whose only positional is a target (download,
		// sponsorblock) widen past category-only detection; a real <id> <id> still
		// goes to arity (below).
		{"download non-category surplus", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock", "blah"}, wantHint},
		{"download typo category surplus", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock", "notacategory"}, wantHint},
		// The flag-first <typo> <url> form (a misspelled category before the target).
		{"download typo natural order", newDownloadCmd, []string{"--sponsorblock", "notacategory", "dummyVideo0"}, wantHint},
		{"sponsorblock typo category surplus", newSponsorBlockCmd, []string{"dummyVideo0", "--sponsorblock", "notacategory"}, wantHint},
		// pflag cannot tell a bare flag from a surplus value, so a stray non-target
		// positional next to --sponsorblock gets the same hint.
		{"download stray arg plus bare flag", newDownloadCmd, []string{"dummyVideo0", "stray", "--sponsorblock"}, wantHint},
		{"download stray arg plus explicit default", newDownloadCmd, []string{"dummyVideo0", "stray", "--sponsorblock=music_offtopic"}, wantHint},
		// A bare flag with no positional is a clean arity error.
		{"download bare flag no positional", newDownloadCmd, []string{"--sponsorblock"}, wantReject},
		{"sponsorblock bare flag no positional", newSponsorBlockCmd, []string{"--sponsorblock"}, wantReject},
		// Two real targets are an arity error, not a misplaced flag value.
		{"download two urls bare flag", newDownloadCmd, []string{"dummyVideo0", "dummyVideo1", "--sponsorblock"}, wantReject},
		{"download equals form", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock=sponsor,intro"}, wantOK},
		{"download bare", newDownloadCmd, []string{"dummyVideo0", "--sponsorblock"}, wantOK},
		{"download no flag", newDownloadCmd, []string{"dummyVideo0"}, wantOK},
		// ExactArgs(1) must still reject a surplus without --sponsorblock.
		{"download surplus no flag", newDownloadCmd, []string{"dummyVideo0", "extra"}, wantReject},
		// With -o set, the flag-first form puts the category list in the first
		// positional slot; it should get the SponsorBlock hint, not a duplicate-output
		// error.
		{"cut natural order with -o", newCutCmd, []string{"--sponsorblock", "sponsor,intro", "in.flac", "-o", "out.mp3"}, wantHint},
		// With -o set, a space-separated value is an impossible surplus.
		{"cut with -o set", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor", "-o", "out.webm"}, wantHint},
		{"cut trailing comma with -o", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor,", "-o", "out.webm"}, wantHint},
		// Without -o, a comma-separated category list occupies the output slot.
		{"cut comma form", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor,intro"}, wantHint},
		// A space-separated value plus an output positional exceeds the budget.
		{"cut comma cats plus output", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor,intro", "out.flac"}, wantHint},
		{"cut single cat plus output", newCutCmd, []string{"in.flac", "--sponsorblock", "sponsor", "out.flac"}, wantHint},
		// A category-named input at the first positional must not be treated as the
		// misplaced value; the surplus is a plain arity error.
		{"cut category-named input not treated", newCutCmd, []string{"sponsor", "a.flac", "b.flac", "--sponsorblock"}, wantReject},
		// The real comma-list value is still caught even when the input is category-named.
		{"cut category-named input with comma value", newCutCmd, []string{"sponsor", "--sponsorblock", "sponsor,intro"}, wantHint},
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

// TestRejectEmptySponsorBlock verifies that an explicitly empty --sponsorblock
// value is rejected while bare, populated, and unset forms still pass.
func TestRejectEmptySponsorBlock(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantReject bool
	}{
		{"explicit empty", []string{"--sponsorblock="}, true},
		{"comma only", []string{"--sponsorblock=,"}, true},
		{"spaced blanks", []string{"--sponsorblock=, ,"}, true},
		{"bare flag", []string{"--sponsorblock"}, false},
		{"real category", []string{"--sponsorblock=sponsor"}, false},
		{"unset", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var val string
			cmd := &cobra.Command{Use: "x"}
			bindSponsorBlockFlag(cmd.Flags(), &val, "test")
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}
			err := rejectEmptySponsorBlock(cmd, val)
			switch {
			case tc.wantReject && (!isUsageError(err) || !strings.Contains(err.Error(), "needs at least one category")):
				t.Errorf("rejectEmptySponsorBlock(%v) = %v, want the empty-category usage error", tc.args, err)
			case !tc.wantReject && err != nil:
				t.Errorf("rejectEmptySponsorBlock(%v) = %v, want nil", tc.args, err)
			}
		})
	}
}

// TestEmptySponsorBlockRejectedByCommands verifies that preview, cut, and
// download reject --sponsorblock= before network work. cut uses a missing source
// on purpose so the flag error must precede source validation.
func TestEmptySponsorBlockRejectedByCommands(t *testing.T) {
	cases := [][]string{
		{"sponsorblock", "dummyVideo0", "--sponsorblock="},
		{"cut", "missing.flac", "--cut-range", "0-1", "--sponsorblock="},
		{"download", "dummyVideo0", "--sponsorblock="},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			root := newRootCmd()
			root.SetArgs(args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "needs at least one category") {
				t.Errorf("%v = %v, want the empty-category rejection", args, err)
			}
		})
	}
}
