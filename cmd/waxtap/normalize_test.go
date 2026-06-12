package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveLoudnessTarget(t *testing.T) {
	newCmd := func() *cobra.Command {
		c := &cobra.Command{Use: "normalize"}
		c.Flags().Float64("target", -14, "")
		c.Flags().Float64("loudness-target", -14, "")
		return c
	}

	t.Run("neither set uses the shared default", func(t *testing.T) {
		if got, err := resolveLoudnessTarget(newCmd(), -14, -14); err != nil || got != -14 {
			t.Errorf("= %v, %v; want -14, nil", got, err)
		}
	})
	t.Run("only --target", func(t *testing.T) {
		c := newCmd()
		mustSet(t, c, "target", "-10")
		if got, err := resolveLoudnessTarget(c, -10, -14); err != nil || got != -10 {
			t.Errorf("= %v, %v; want -10, nil", got, err)
		}
	})
	t.Run("only --loudness-target", func(t *testing.T) {
		c := newCmd()
		mustSet(t, c, "loudness-target", "-9")
		if got, err := resolveLoudnessTarget(c, -14, -9); err != nil || got != -9 {
			t.Errorf("= %v, %v; want -9, nil", got, err)
		}
	})
	t.Run("both set to the same value", func(t *testing.T) {
		c := newCmd()
		mustSet(t, c, "target", "-8")
		mustSet(t, c, "loudness-target", "-8")
		if got, err := resolveLoudnessTarget(c, -8, -8); err != nil || got != -8 {
			t.Errorf("= %v, %v; want -8, nil", got, err)
		}
	})
	t.Run("conflicting values are rejected", func(t *testing.T) {
		c := newCmd()
		mustSet(t, c, "target", "-8")
		mustSet(t, c, "loudness-target", "-12")
		if _, err := resolveLoudnessTarget(c, -8, -12); err == nil {
			t.Error("conflicting --target/--loudness-target should be rejected")
		}
	})
}
