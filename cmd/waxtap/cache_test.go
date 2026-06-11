package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCacheUnknownSubcommand(t *testing.T) {
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"bogus"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("cache bogus should return an error, not exit 0")
	}
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Fatalf("err = %v (%T), want *usageError (exit 2)", err, err)
	}
	if !strings.Contains(err.Error(), "unknown cache subcommand") {
		t.Errorf("err = %q, want it to name the unknown subcommand", err)
	}
}

func TestCacheNoArgsPrintsHelp(t *testing.T) {
	cmd := newCacheCmd()
	cmd.SetArgs(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare cache should print help and succeed, got %v", err)
	}
	if !strings.Contains(out.String(), "cache") {
		t.Errorf("help output = %q, want it to mention cache usage", out.String())
	}
}
