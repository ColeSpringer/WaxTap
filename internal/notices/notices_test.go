package notices

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"Permission is hereby granted, free of charge, to any person obtaining a copy":                                           "MIT",
		"Apache License\n  Version 2.0, January 2004":                                                                            "Apache-2.0",
		"Redistribution and use in source and binary forms ... Neither the name of Google":                                       "BSD-3-Clause",
		"Redistribution and use in source and binary forms, with or without modification ...":                                    "BSD-2-Clause",
		"Redistribution and use in source and binary forms ... All advertising materials mentioning features":                    "BSD-4-Clause",
		"Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted": "ISC",
		"Mozilla Public License, version 2.0":                                                                                    "MPL-2.0",
		"Boost Software License - Version 1.0 ... Permission is hereby granted, free of charge":                                  "BSL-1.0",
		"This is free and unencumbered software released into the public domain.":                                                "Unlicense",
		"All rights reserved. Do not redistribute.":                                                                              "",
		"Permission is hereby granted, free of charge ... or under the Apache License, Version 2.0 at your option":               "",
	}
	for text, want := range cases {
		if got := classify(text); got != want {
			t.Errorf("classify(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestFileKind(t *testing.T) {
	cases := map[string]kind{
		"LICENSE": kindLicense, "LICENSE.txt": kindLicense, "LICENCE.md": kindLicense, "COPYING": kindLicense,
		"license-MIT": kindLicense, "LICENSE_V8": kindLicense, "MIT-LICENSE.txt": kindLicense, "UNLICENSE": kindLicense,
		"LICENSE.APACHE": kindLicense, "COPYRIGHT": kindLicense,
		"NOTICE": kindNotice, "NOTICE.txt": kindNotice, "THIRD-PARTY-NOTICES.md": kindNotice, "third_party_notices.txt": kindNotice,
		"PATENTS": kindOther, "README.md": kindOther, "go.mod": kindOther, "AUTHORS": kindOther,
		// Source files under a license-like name are code, not terms.
		"license.go": kindOther, "notice.c": kindOther, "licenses.py": kindOther, "license.json": kindOther,
	}
	for name, want := range cases {
		if got := fileKind(name); got != want {
			t.Errorf("fileKind(%q) = %v, want %v", name, got, want)
		}
	}
}

// collect takes the license and notice files at a module root and beside each
// linked package, in path order, classifies the licenses, and leaves patents,
// documentation, source files, and directories no linked package lives in (the
// pprof third_party tree) alone.
func TestCollect(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("LICENSE", "MIT License\r\n\r\nPermission is hereby granted, free of charge, ...\r\n")
	write("NOTICE", "Product includes software developed elsewhere.\n")
	write("THIRD-PARTY-NOTICES.md", "# Third-party notices\n")
	write("PATENTS", "Additional IP Rights Grant\n")
	write("README.md", "# readme\n")
	write("license.go", "package m\n")
	write("ftoa/LICENSE_LUCENE", "Apache License Version 2.0\n")
	write("ftoa/internal/fast/LICENSE_V8", "Redistribution and use in source and binary forms ... Neither the name of Google\n")
	write("third_party/LICENSE", "Apache License Version 2.0\n")

	files, err := collect(dir, []string{filepath.Join(dir, "ftoa"), filepath.Join(dir, "ftoa", "internal", "fast"), dir})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if want := "LICENSE NOTICE THIRD-PARTY-NOTICES.md ftoa/LICENSE_LUCENE ftoa/internal/fast/LICENSE_V8"; strings.Join(paths, " ") != want {
		t.Fatalf("files = %v, want %q", paths, want)
	}
	if files[0].License != "MIT" || files[0].Notice {
		t.Errorf("LICENSE = %+v, want MIT and not a notice", files[0])
	}
	if strings.Contains(files[0].Text, "\r") {
		t.Error("CRLF line endings were not normalized")
	}
	for _, f := range files[1:3] {
		if !f.Notice || f.License != "" {
			t.Errorf("%s = notice %v license %q, want a notice with no license class", f.Path, f.Notice, f.License)
		}
	}
	if files[3].License != "Apache-2.0" || files[4].License != "BSD-3-Clause" {
		t.Errorf("nested licenses = %q, %q; want Apache-2.0, BSD-3-Clause", files[3].License, files[4].License)
	}
	if _, err := collect(filepath.Join(dir, "missing"), nil); err == nil {
		t.Error("collect on a missing directory = nil error")
	}
	if _, err := collect(filepath.Join(dir, "ftoa"), []string{dir}); err == nil {
		t.Error("collect with a package directory outside the root = nil error")
	}
}

// A module with a notice but no license has terms nobody stated; it fails
// generation rather than rendering a cell with no license in it.
func TestCheckFilesRequiresALicense(t *testing.T) {
	m := Module{Path: "example.com/m", Version: "v1", Files: []File{{Path: "NOTICE", Notice: true}}}
	if err := checkFiles(m); err == nil || !strings.Contains(err.Error(), "no license file") {
		t.Errorf("checkFiles(notice only) = %v, want a no-license error", err)
	}
	m.Files = append(m.Files, File{Path: "COPYING", Text: "odd text"})
	if err := checkFiles(m); err != nil {
		t.Errorf("checkFiles(with an unclassified license) = %v, want nil", err)
	}
}

// A text containing a code fence is fenced with a longer one, so it cannot
// close the block early and turn the rest of the file into prose.
func TestFenceOutlivesBackticks(t *testing.T) {
	var b strings.Builder
	fence(&b, "before\n```\ninside\n```\nafter\n")
	got := b.String()
	if !strings.HasPrefix(got, "````text\n") || !strings.HasSuffix(got, "\n````\n") {
		t.Errorf("fence = %q, want four-backtick delimiters", got)
	}
	b.Reset()
	fence(&b, "plain\n\n\n")
	if got := b.String(); got != "```text\nplain\n```\n" {
		t.Errorf("fence = %q, want trailing newlines trimmed inside a three-backtick fence", got)
	}
}

func TestRenderListsEveryModuleAndFile(t *testing.T) {
	mods := []Module{
		{Path: "example.com/a", Version: "v1.0.0", Files: []File{
			{Path: "LICENSE", Text: "mit text", License: "MIT"},
			{Path: "THIRD-PARTY-NOTICES.md", Text: "ported code", Notice: true},
			{Path: "sub/LICENSE_V8", Text: "v8 text", License: "BSD-3-Clause"},
		}},
		{Path: "example.com/b", Version: "v2.0.0", Files: []File{{Path: "COPYING", Text: "odd text"}}},
	}
	out := string(render("example.com/cmd/x", ReleaseTargets, "1.26", "Copyright 2009 The Go Authors.\nRedistribution and use in source and binary forms ... Neither the name", mods))
	// The preamble wraps; only the table and fenced texts may run long.
	for i, line := range strings.Split(strings.SplitN(out, "\n| ", 2)[0], "\n") {
		if len(line) > 78 && !strings.Contains(line, "`example.com/cmd/x`") {
			t.Errorf("preamble line %d is %d columns: %q", i+1, len(line), line)
		}
	}
	for _, want := range []string{
		"| Go runtime and standard library | go 1.26 | BSD-3-Clause |",
		"| example.com/a | v1.0.0 | MIT / BSD-3-Clause (see also THIRD-PARTY-NOTICES.md) |",
		"| example.com/b | v2.0.0 | see text |",
		"## example.com/a v1.0.0",
		"License: MIT (LICENSE)",
		"### THIRD-PARTY-NOTICES.md",
		"ported code",
		"License: BSD-3-Clause (sub/LICENSE_V8)",
		"License: see text (COPYING)",
		"odd text",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered notices lack %q", want)
		}
	}
}

// repoFile reads a file at the module root and, by reading it through the
// test binary, makes it an input of the test cache: a change to it reruns the
// test instead of replaying a cached pass.
func repoFile(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The checked-in file is exactly what the current go.mod produces, so a
// dependency bump cannot ship without its notices.
//
// This package imports nothing outside the standard library, so a bump does
// not rebuild its test binary; go.mod and go.sum are read here so the test
// cache sees the bump and reruns rather than replaying a pass from before it.
// The generation itself runs go list, which downloads a module it lacks (the
// Windows-only ones on another host), so an offline run fails rather than
// skips: a currency check that skips is not one.
func TestCheckedInFileIsCurrent(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go command is not on PATH")
	}
	repoFile(t, "go.mod")
	repoFile(t, "go.sum")
	got, err := Generate(context.Background(), BinaryPackage, ReleaseTargets)
	if err != nil {
		t.Fatal(err)
	}
	want := repoFile(t, FileName)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate it with: go run ./internal/notices/gen", FileName)
	}
	// The release binaries' own modules and the ported code inside them must be
	// among what the file carries.
	for _, sect := range []string{
		"\n## github.com/colespringer/waxflow ",
		"\n## github.com/spf13/cobra ",
		"\n## github.com/inconshreveable/mousetrap ",
		"\n### THIRD-PARTY-NOTICES.md\n",
		"(ftoa/LICENSE_LUCENE)",
		"(ftoa/internal/fast/LICENSE_V8)",
	} {
		if !bytes.Contains(got, []byte(sect)) {
			t.Errorf("the notices lack %q", sect)
		}
	}
}

// ReleaseTargets and BinaryPackage restate .goreleaser.yaml's build matrix and
// main package; this reads the config so the two cannot drift.
func TestReleaseTargetsMatchGoreleaser(t *testing.T) {
	cfg := string(repoFile(t, ".goreleaser.yaml"))
	lists := map[string][]string{}
	var main string
	key := ""
	for _, line := range strings.Split(cfg, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "main:"):
			main = strings.TrimSpace(strings.TrimPrefix(trim, "main:"))
		case trim == "goos:" || trim == "goarch:":
			key = strings.TrimSuffix(trim, ":")
		case key != "" && strings.HasPrefix(trim, "- "):
			lists[key] = append(lists[key], strings.TrimSpace(strings.TrimPrefix(trim, "- ")))
		case key != "" && trim != "" && !strings.HasPrefix(trim, "#"):
			key = ""
		}
	}
	if len(lists["goos"]) == 0 || len(lists["goarch"]) == 0 || main == "" {
		t.Fatalf("could not read the build matrix from .goreleaser.yaml: goos=%v goarch=%v main=%q", lists["goos"], lists["goarch"], main)
	}
	want := map[Target]bool{}
	for _, os := range lists["goos"] {
		for _, arch := range lists["goarch"] {
			want[Target{os, arch}] = true
		}
	}
	got := map[Target]bool{}
	for _, tg := range ReleaseTargets {
		got[tg] = true
	}
	for tg := range want {
		if !got[tg] {
			t.Errorf(".goreleaser.yaml builds %s/%s but ReleaseTargets lacks it", tg.OS, tg.Arch)
		}
	}
	for tg := range got {
		if !want[tg] {
			t.Errorf("ReleaseTargets lists %s/%s, which .goreleaser.yaml does not build", tg.OS, tg.Arch)
		}
	}
	modLine, _, _ := strings.Cut(string(repoFile(t, "go.mod")), "\n")
	module := strings.TrimSpace(strings.TrimPrefix(modLine, "module"))
	if want := module + "/" + strings.TrimPrefix(main, "./"); BinaryPackage != want {
		t.Errorf("BinaryPackage = %q, want %q from .goreleaser.yaml's main %q", BinaryPackage, want, main)
	}
}
