package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/diskcache"
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

func TestCacheCleanRefusesAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "precious.txt")
	if err := os.WriteFile(file, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"clean", "--cache-dir", file})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Fatalf("err = %v, want a usage error for a path that is not a directory", err)
	}
	if _, serr := os.Stat(file); serr != nil {
		t.Fatal("cache clean removed a regular file")
	}
}

func TestCacheCleanLeavesAForeignDirectoryAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notacache")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "data.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"clean", "--cache-dir", dir})
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "sub", "data.bin")); serr != nil {
		t.Fatal("cache clean removed a directory holding nothing of WaxTap's")
	}
	if !strings.Contains(out.String(), "nothing to clean") {
		t.Errorf("stdout = %q, want the nothing-to-clean line", out.String())
	}
}

// A stray file beside the entry directories must not make a cache
// unremovable: requiring every entry to be a schema directory meant one
// .DS_Store refused to clean WaxTap's own cache for good.
func TestCacheCleanToleratesAStrayFileBesideTheEntries(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cache")
	players := filepath.Join(base, "players")
	if err := os.MkdirAll(filepath.Join(players, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(players, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"clean", "--cache-dir", base})
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(players); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("the cache survived a stray dotfile: %v", serr)
	}
	// The line says what happened: the entries always go, the directory only
	// when that emptied it.
	if !strings.Contains(out.String(), "cleaned") {
		t.Errorf("stdout = %q, want the cleaned line", out.String())
	}
}

// The base survives when it holds anything else, so the line must not claim
// the directory was removed.
func TestCacheCleanSaysCleanedWhenTheBaseSurvives(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cache")
	diskcache.New(diskcache.Options{Dir: filepath.Join(base, "players"), SchemaVersion: 1}).Put("k", []byte("v"))
	keep := filepath.Join(base, "notes.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"clean", "--cache-dir", base})
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(keep); serr != nil {
		t.Fatalf("a file beside the cache was removed: %v", serr)
	}
	if _, serr := os.Stat(base); serr != nil {
		t.Fatalf("the base was removed while it still held a file: %v", serr)
	}
	if strings.Contains(out.String(), "removed "+base) {
		t.Errorf("stdout = %q, want it not to claim the directory was removed", out.String())
	}
}

func TestCacheCleanRemovesAPopulatedCache(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cache")
	s := diskcache.New(diskcache.Options{Dir: filepath.Join(base, "players"), SchemaVersion: 1})
	s.Put("k", []byte("v"))
	if _, err := os.Stat(filepath.Join(base, "players", diskcache.TagFile)); err != nil {
		t.Fatalf("Put did not write the tag: %v", err)
	}
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"clean", "--cache-dir", base})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(base); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("cache dir still present: %v", serr)
	}
}
