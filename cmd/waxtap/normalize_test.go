package main

import (
	"bytes"
	"errors"
	"testing"
)

func TestNormalizeFlagSurface(t *testing.T) {
	flags := newNormalizeCmd().Flags()
	for _, name := range []string{"apply", "loudness-target"} {
		if flags.Lookup(name) == nil {
			t.Errorf("--%s is missing", name)
		}
	}
	for _, name := range []string{"target", "measure", "force"} {
		if flags.Lookup(name) != nil {
			t.Errorf("--%s should not be exposed", name)
		}
	}
}

func TestNormalizeMeasureRejectsWriteFlags(t *testing.T) {
	for _, args := range [][]string{
		{"in.wav", "--loudness-target", "-16"},
		{"in.wav", "--format", "flac"},
		{"in.wav", "--bitrate", "128000"},
		{"in.wav", "--out", "out.flac"},
		{"in.wav", "--dir", "out"},
		{"in.wav", "--collision", "overwrite"},
		{"in.wav", "--downmix"},
		{"in.wav", "--channels", "mono"},
		{"in.wav", "out.flac"},
	} {
		assertNormalizeUsageError(t, args)
	}
}

func TestNormalizeRejectsInputSpecificFlags(t *testing.T) {
	for _, args := range [][]string{
		{"in.wav", "--apply", "--format", "flac", "--recursive"},
		{"in.wav", "--apply", "--format", "flac", "--concurrency", "2"},
		{"in.wav", "--apply", "--format", "flac", "--dir", "out"},
		{"a.wav", "b.wav", "--album", "--apply", "--format", "flac", "--dir", "out", "--recursive"},
		{"a.wav", "b.wav", "--album", "--apply", "--format", "flac", "--dir", "out", "--channels", "mono"},
	} {
		assertNormalizeUsageError(t, args)
	}
}

func assertNormalizeUsageError(t *testing.T, args []string) {
	t.Helper()
	cmd := newNormalizeCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("normalize %v: err = %v (%T), want *usageError (exit 2)", args, err, err)
	}
}
