// Package notices renders THIRD-PARTY-NOTICES.md, the attribution file the
// release archives ship beside LICENSE.
//
// Every release binary compiles in the Go modules it depends on and the Go
// runtime, and their licenses (MIT, BSD, Apache-2.0) require that the copyright
// and permission notices travel with a binary distribution. The file is
// generated rather than hand-kept so a dependency bump cannot silently leave a
// module out: Generate lists the modules go list links into the binary on each
// release target, reads the license and notice files at each module's root and
// beside each linked package out of the module cache, and reproduces them
// verbatim. A test compares the checked-in file against a fresh render, so the
// file is exactly what go.mod says it should be.
//
//go:generate go run ./gen
package notices

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// BinaryPackage is the import path of the released binary.
const BinaryPackage = "github.com/colespringer/waxtap/v3/cmd/waxtap"

// FileName is the notices file's name at the module root, the name
// .goreleaser.yaml ships.
const FileName = "THIRD-PARTY-NOTICES.md"

// Target is one GOOS/GOARCH pair a release binary is built for.
type Target struct{ OS, Arch string }

// ReleaseTargets mirrors the goos/goarch matrix in .goreleaser.yaml, which
// TestReleaseTargetsMatchGoreleaser holds it to. A module linked on any of
// them is listed: cobra pulls in mousetrap on Windows only, and the Windows
// archive is a release archive too.
var ReleaseTargets = []Target{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

// Module is one dependency compiled into the binary, with the license and
// notice files found at its root and beside the packages of it the binary
// links.
type Module struct {
	Path    string
	Version string
	Dir     string
	// Files are the module's license and notice files in path order. Every
	// module the binary links must have at least one license file, or Generate
	// fails: a dependency with no license text is a problem to look at, not to
	// ship.
	Files []File
}

// File is one license or notice file, reproduced verbatim.
type File struct {
	// Path is relative to the module root: "LICENSE", "ftoa/LICENSE_LUCENE".
	Path string
	Text string
	// License is the classification of a license file ("MIT", "BSD-3-Clause",
	// ...), "" for a notice file or a text the classifier does not recognize.
	License string
	// Notice marks a NOTICE or THIRD-PARTY-NOTICES file: attributions the
	// module itself owes for code it ports, which a binary embedding the
	// module owes in turn.
	Notice bool
}

// goTool locates the go command: the one on PATH (go test puts GOROOT/bin
// there), else the toolchain that built this binary.
func goTool() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	if root := os.Getenv("GOROOT"); root != "" {
		p := filepath.Join(root, "bin", "go"+exeSuffix())
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("notices: the go command is not on PATH")
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// run executes one go command. GOWORK=off keeps a developer's workspace out
// of the answer: with a go.work in force every workspace module is Main, so
// its dependency edges would be dropped and `go list -m` would print one line
// per module, while the release build has no workspace. A failure carries the
// command's stderr, which is where go says what is missing.
func run(ctx context.Context, tool string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = append(append(os.Environ(), "GOWORK=off"), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("notices: go %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ModuleRoot returns the main module's directory, where the notices file lives.
func ModuleRoot(ctx context.Context) (string, error) {
	tool, err := goTool()
	if err != nil {
		return "", err
	}
	out, err := run(ctx, tool, nil, "list", "-m", "-f", "{{.Dir}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Generate renders the notices file for the binary built from pkg (an import
// path), covering every module linked on any of targets.
func Generate(ctx context.Context, pkg string, targets []Target) ([]byte, error) {
	tool, err := goTool()
	if err != nil {
		return nil, err
	}
	mods, pkgDirs, err := linkedModules(ctx, tool, pkg, targets)
	if err != nil {
		return nil, err
	}
	for i := range mods {
		if mods[i].Files, err = collect(mods[i].Dir, pkgDirs[mods[i].Path]); err != nil {
			return nil, err
		}
		if err := checkFiles(mods[i]); err != nil {
			return nil, err
		}
	}
	goVersion, goLicense, err := runtimeLicense(ctx, tool)
	if err != nil {
		return nil, err
	}
	return render(pkg, targets, goVersion, goLicense, mods), nil
}

// listPackage is the subset of go list -json a dependency walk needs.
type listPackage struct {
	Dir      string
	Standard bool
	Module   *struct {
		Path    string
		Version string
		Dir     string
		Main    bool
	}
}

// linkedModules unions the non-main modules go list -deps reports for pkg
// across targets, sorted by path, with the directories of the packages the
// binary links from each.
func linkedModules(ctx context.Context, tool, pkg string, targets []Target) ([]Module, map[string][]string, error) {
	seen := map[string]Module{}
	pkgDirs := map[string]map[string]bool{}
	for _, t := range targets {
		out, err := run(ctx, tool, []string{"GOOS=" + t.OS, "GOARCH=" + t.Arch}, "list", "-deps", "-json=Dir,Standard,Module", pkg)
		if err != nil {
			return nil, nil, fmt.Errorf("%w (target %s/%s)", err, t.OS, t.Arch)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p listPackage
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				return nil, nil, fmt.Errorf("notices: decoding go list output: %w", err)
			}
			if p.Standard || p.Module == nil || p.Module.Main {
				continue
			}
			if p.Module.Dir == "" {
				return nil, nil, fmt.Errorf("notices: %s@%s is not in the module cache; run go mod download", p.Module.Path, p.Module.Version)
			}
			seen[p.Module.Path] = Module{Path: p.Module.Path, Version: p.Module.Version, Dir: p.Module.Dir}
			if pkgDirs[p.Module.Path] == nil {
				pkgDirs[p.Module.Path] = map[string]bool{}
			}
			pkgDirs[p.Module.Path][p.Dir] = true
		}
	}
	mods := make([]Module, 0, len(seen))
	for _, m := range seen {
		mods = append(mods, m)
	}
	slices.SortFunc(mods, func(a, b Module) int { return strings.Compare(a.Path, b.Path) })
	dirs := make(map[string][]string, len(pkgDirs))
	for path, set := range pkgDirs {
		for dir := range set {
			dirs[path] = append(dirs[path], dir)
		}
		slices.Sort(dirs[path])
	}
	return mods, dirs, nil
}

// collect reads the license and notice files at a module root and in each of
// pkgDirs, the directories of the module's packages the binary links: a module
// that ports code keeps that code's license beside it (goja's ftoa carries
// Lucene's and V8's), and the binary owes those notices as much as the root
// one. Directories no linked package lives in are not read, which is what
// keeps pprof's third_party tree, the licenses of a web UI the binary never
// links, out. Paths are relative to the root, in path order.
func collect(root string, pkgDirs []string) ([]File, error) {
	var files []File
	seen := map[string]bool{}
	for _, dir := range append([]string{root}, pkgDirs...) {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("notices: %w", err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			kind := fileKind(e.Name())
			if kind == kindOther {
				continue
			}
			full := filepath.Join(dir, e.Name())
			rel, err := filepath.Rel(root, full)
			if err != nil || strings.HasPrefix(rel, "..") {
				return nil, fmt.Errorf("notices: %s is outside its module root %s", full, root)
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				return nil, fmt.Errorf("notices: %w", err)
			}
			text := strings.ReplaceAll(string(raw), "\r\n", "\n")
			f := File{Path: filepath.ToSlash(rel), Text: text, Notice: kind == kindNotice}
			if kind == kindLicense {
				f.License = classify(text)
			}
			files = append(files, f)
		}
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

// checkFiles requires a license file: a module whose only text is a NOTICE
// has an attribution with no terms behind it, which is a module to look at
// rather than list.
func checkFiles(m Module) error {
	for _, f := range m.Files {
		if !f.Notice {
			return nil
		}
	}
	return fmt.Errorf("notices: %s@%s has no license file at its root or beside its linked packages (%s)", m.Path, m.Version, m.Dir)
}

type kind int

const (
	kindOther kind = iota
	kindLicense
	kindNotice
)

// codeExts are the extensions of source files a module root can plausibly
// hold under a license-like name (a license.go in a package that models
// licenses, say). Text extensions and unknown ones (LICENSE.APACHE) are judged
// by the name.
var codeExts = map[string]bool{
	".go": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
	".js": true, ".ts": true, ".py": true, ".rs": true, ".java": true, ".rb": true,
	".sh": true, ".bat": true, ".ps1": true, ".pl": true, ".php": true, ".swift": true,
	".m": true, ".mm": true, ".kt": true, ".cs": true, ".s": true, ".asm": true,
	".html": true, ".css": true, ".json": true, ".yaml": true, ".yml": true,
	".toml": true, ".xml": true, ".proto": true,
}

// fileKind classifies a file name: a license (LICENSE, LICENCE, COPYING,
// COPYRIGHT, UNLICENSE, LICENSE_V8, MIT-LICENSE.txt), a notice (NOTICE,
// THIRD-PARTY-NOTICES.md), or neither. PATENTS files are grants to the user,
// not notices a distribution must carry, and AUTHORS lists are not license
// texts; the copyright line every license carries covers them.
func fileKind(name string) kind {
	n := strings.ToLower(name)
	ext := filepath.Ext(n)
	if codeExts[ext] {
		return kindOther
	}
	base := strings.TrimSuffix(n, ext)
	switch {
	case strings.HasPrefix(base, "notice"), strings.Contains(base, "third-party"), strings.Contains(base, "third_party"), strings.Contains(base, "thirdparty"):
		return kindNotice
	case strings.Contains(base, "licen"), strings.HasPrefix(base, "copying"), strings.HasPrefix(base, "copyright"):
		return kindLicense
	}
	return kindOther
}

var spaces = regexp.MustCompile(`\s+`)

// classify names a license from the phrases that identify it, or returns ""
// when it recognizes none or more than one (a file that carries two licenses,
// or mentions another's name in its own text). The full text is reproduced
// either way; the name is a reading aid for the summary table, and a text the
// classifier cannot name with confidence is listed as "see text" rather than
// guessed at.
func classify(text string) string {
	t := strings.ToLower(spaces.ReplaceAllString(text, " "))
	var found []string
	add := func(name string, ok bool) {
		if ok {
			found = append(found, name)
		}
	}
	add("Apache-2.0", strings.Contains(t, "apache license") && strings.Contains(t, "version 2.0"))
	add("MPL-2.0", strings.Contains(t, "mozilla public license") && strings.Contains(t, "2.0"))
	boost := strings.Contains(t, "boost software license")
	add("BSL-1.0", boost)
	add("MIT", strings.Contains(t, "permission is hereby granted, free of charge") && !boost)
	add("ISC", strings.Contains(t, "permission to use, copy, modify, and/or distribute this software for any purpose with or without fee"))
	add("Unlicense", strings.Contains(t, "this is free and unencumbered software released into the public domain"))
	if strings.Contains(t, "redistribution and use in source and binary forms") {
		switch {
		case strings.Contains(t, "advertising materials"):
			add("BSD-4-Clause", true)
		case strings.Contains(t, "neither the name") || strings.Contains(t, "may not be used to endorse or promote"):
			add("BSD-3-Clause", true)
		default:
			add("BSD-2-Clause", true)
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// runtimeLicense returns the go directive of the main module and the
// toolchain's LICENSE, which covers the runtime and standard library compiled
// into every binary. The directive names the release line without its patch
// level: GOTOOLCHAIN holds every build to at least that line, and the LICENSE
// text does not change within one, so the file does not churn per patch
// release.
func runtimeLicense(ctx context.Context, tool string) (version, text string, err error) {
	out, err := run(ctx, tool, nil, "list", "-m", "-f", "{{.GoVersion}}")
	if err != nil {
		return "", "", err
	}
	version = strings.TrimSpace(string(out))
	root, err := run(ctx, tool, nil, "env", "GOROOT")
	if err != nil {
		return "", "", err
	}
	raw, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(root)), "LICENSE"))
	if err != nil {
		return "", "", fmt.Errorf("notices: reading the Go toolchain LICENSE: %w", err)
	}
	return version, strings.ReplaceAll(string(raw), "\r\n", "\n"), nil
}

// render writes the Markdown: a preamble, a summary table, then every file
// verbatim inside a fence.
func render(pkg string, targets []Target, goVersion, goLicense string, mods []Module) []byte {
	var b strings.Builder
	b.WriteString("# Third-party notices\n\n")
	paragraph(&b, "The release binaries compile in the Go modules below and the Go runtime. "+
		"Their licenses require the copyright and permission notices to travel with a "+
		"binary distribution, so this file ships in every release archive beside "+
		"LICENSE. WaxTap itself is MIT licensed; see LICENSE.")
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.OS + "/" + t.Arch
	}
	paragraph(&b, "Generated from the module cache by `go run ./internal/notices/gen` for the "+
		"binary `"+pkg+"` on the release targets "+strings.Join(names, ", ")+"; a module "+
		"linked on any of them is listed, with the license files at its root and beside "+
		"the packages the binary links. Regenerate after a dependency change; the tests "+
		"in internal/notices fail while this file is stale. Every license and notice text "+
		"is reproduced verbatim.")

	b.WriteString("| Component | Version | License |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Go runtime and standard library | go %s | %s |\n", goVersion, classifyOrSeeText(goLicense))
	for _, m := range mods {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", m.Path, m.Version, summarize(m))
	}

	b.WriteString("\n## Go runtime and standard library\n\n")
	fmt.Fprintf(&b, "License: %s (GOROOT/LICENSE)\n\n", classifyOrSeeText(goLicense))
	fence(&b, goLicense)
	for _, m := range mods {
		fmt.Fprintf(&b, "\n## %s %s\n", m.Path, m.Version)
		for _, f := range m.Files {
			switch {
			case f.Notice:
				fmt.Fprintf(&b, "\n### %s\n\nThird-party attributions the module carries, reproduced verbatim.\n\n", f.Path)
			case f.License != "":
				fmt.Fprintf(&b, "\nLicense: %s (%s)\n\n", f.License, f.Path)
			default:
				fmt.Fprintf(&b, "\nLicense: see text (%s)\n\n", f.Path)
			}
			fence(&b, f.Text)
		}
	}
	return []byte(b.String())
}

func classifyOrSeeText(text string) string {
	if c := classify(text); c != "" {
		return c
	}
	return "see text"
}

// summarize is the table cell: the license names in path order, then a pointer
// at any notice file, since a reader deciding what a binary carries needs both.
func summarize(m Module) string {
	var parts []string
	var notices []string
	for _, f := range m.Files {
		switch {
		case f.Notice:
			notices = append(notices, f.Path)
		case f.License != "":
			parts = append(parts, f.License)
		default:
			parts = append(parts, "see text")
		}
	}
	s := strings.Join(parts, " / ")
	if len(notices) > 0 {
		s += " (see also " + strings.Join(notices, ", ") + ")"
	}
	return s
}

// paragraph writes text word-wrapped at 78 columns, then a blank line. A word
// longer than that (an import path) takes a line of its own.
func paragraph(b *strings.Builder, text string) {
	const width = 78
	col := 0
	for _, w := range strings.Fields(text) {
		switch {
		case col == 0:
		case col+1+len(w) > width:
			b.WriteString("\n")
			col = 0
		default:
			b.WriteString(" ")
			col++
		}
		b.WriteString(w)
		col += len(w)
	}
	b.WriteString("\n\n")
}

// fence writes text inside a code fence long enough that no backtick run in
// the text can close it early.
func fence(b *strings.Builder, text string) {
	f := "```"
	for strings.Contains(text, f) {
		f += "`"
	}
	b.WriteString(f)
	b.WriteString("text\n")
	b.WriteString(strings.TrimRight(text, "\n"))
	b.WriteString("\n")
	b.WriteString(f)
	b.WriteString("\n")
}
