package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestArchiveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.txt")

	a, err := openArchive(path) // missing file is fine
	if err != nil {
		t.Fatal(err)
	}
	if a.Has("abc") {
		t.Error("empty archive should not contain abc")
	}
	if err := a.Add("abc"); err != nil {
		t.Fatal(err)
	}
	if !a.Has("abc") {
		t.Error("after Add, Has should be true")
	}
	if err := a.Add("abc"); err != nil { // duplicate is a no-op
		t.Fatal(err)
	}

	// A fresh handle must load the persisted ID.
	b, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Has("abc") {
		t.Error("reopened archive lost abc")
	}

	// The archive uses yt-dlp's "youtube <id>" line format.
	data, _ := os.ReadFile(path)
	if string(data) != "youtube abc\n" {
		t.Errorf("archive contents = %q, want %q", data, "youtube abc\n")
	}
}

func TestArchiveID(t *testing.T) {
	cases := map[string]string{
		"abc":         "abc",
		"youtube abc": "abc",
		"  abc  ":     "abc",
		"# comment":   "",
		"":            "",
	}
	for in, want := range cases {
		if got := archiveID(in); got != want {
			t.Errorf("archiveID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArchiveConcurrentAdd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.txt")
	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = a.Add(string(rune('a'+i%26)) + "0000000000")
		}(i)
	}
	wg.Wait()
	// 26 distinct IDs should be recorded exactly once each.
	b, _ := openArchive(path)
	for i := 0; i < 26; i++ {
		if !b.Has(string(rune('a'+i)) + "0000000000") {
			t.Errorf("missing id index %d", i)
		}
	}
}

// The archive is the record of what not to download again, so a failed append
// must not be remembered as a success: the next run would skip a video it never
// wrote down.
func TestArchiveAddReportsAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "archive.txt") // no such directory
	a, err := openArchive(path)
	if err != nil {
		t.Fatalf("openArchive: %v", err)
	}
	if err := a.Add("dummyVideo0"); err == nil {
		t.Fatal("Add on an unwritable path returned no error")
	}
	if a.seen["dummyVideo0"] {
		t.Error("a failed append was remembered; the next run would skip the video")
	}
	// The failure is not sticky: a working path records as usual.
	ok := filepath.Join(dir, "archive.txt")
	b, err := openArchive(ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Add("dummyVideo1"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !b.seen["dummyVideo1"] {
		t.Error("a successful append was not remembered")
	}
}
