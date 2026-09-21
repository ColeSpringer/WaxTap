package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// closeCountingMedia reports its Close to the test, so the test can count how
// many members the engine holds open at once. It embeds the interface alone,
// the shape closingMedia has, so a member is measured through exactly what
// AnalyzeGroup hands the engine.
type closeCountingMedia struct {
	format.Media
	onClose func()
}

func (m *closeCountingMedia) Close() error {
	m.onClose()
	return m.Media.Close()
}

// memberLedger wraps openFileMedia and records what the engine opened, in
// what order, and how many were held open at once. (A field and a method
// cannot share a name in Go, hence held rather than open.)
type memberLedger struct {
	opened     []string
	held, peak int
}

func (l *memberLedger) open(path, hint string) (format.Media, error) {
	m, err := openFileMedia(path, hint)
	if err != nil {
		return nil, err
	}
	l.opened = append(l.opened, filepath.Base(path))
	l.held++
	l.peak = max(l.peak, l.held)
	return &closeCountingMedia{Media: m, onClose: func() { l.held-- }}, nil
}

func writeTones(t *testing.T, dir string, n int) []string {
	t.Helper()
	paths := make([]string, 0, n)
	for i := range n {
		paths = append(paths, writeFixture(t, dir, fmt.Sprintf("t%d.wav", i), mediatest.SineWAV(1, 2)))
	}
	return paths
}

// tornADTS writes a whole ADTS stream and a copy cut off at 60%, the fixture
// TestAnalyzeFileReportsReadDamage uses: the torn one probes clean and only
// a read that reaches the torn frame reports it.
func tornADTS(t *testing.T, r *Runner, dir string) (whole, torn string) {
	t.Helper()
	wav := writeFixture(t, dir, "src.wav", mediatest.SineWAV(3, 2))
	whole = filepath.Join(dir, "whole.aac")
	if _, err := r.Transcode(context.Background(), wav, whole, Spec{Codec: CodecAAC}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(whole)
	if err != nil {
		t.Fatal(err)
	}
	return whole, writeFixture(t, dir, "torn.aac", data[:len(data)*6/10])
}

// An album of N tracks holds one descriptor at a time: each member is opened
// when the engine reaches it and closed before the next opens, in input
// order, and none is left open after the call.
func TestAnalyzeGroupOpensMembersOneAtATime(t *testing.T) {
	inputs := writeTones(t, t.TempDir(), 3)
	r := NewRunner(RunnerConfig{})
	var l memberLedger
	r.openMember = l.open
	group, members, err := r.AnalyzeGroup(context.Background(), inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t0.wav", "t1.wav", "t2.wav"}; !slices.Equal(l.opened, want) {
		t.Errorf("opened %v, want %v", l.opened, want)
	}
	if l.peak != 1 {
		t.Errorf("%d members were open at once, want one at a time", l.peak)
	}
	if l.held != 0 {
		t.Errorf("%d members still open after the call", l.held)
	}
	if group == nil || len(members) != 3 {
		t.Fatalf("group %v, %d members, want a group of 3", group, len(members))
	}
}

// A member the filesystem refuses is reported before any member is decoded:
// the error names the member index the way the engine's own errors do, and a
// file that would not open stays an I/O failure with its path rather than
// bad input. Nothing is decoded first, so an unreadable last track does not
// cost the album ahead of it.
func TestAnalyzeGroupStopsAtTheFirstUnopenableMember(t *testing.T) {
	dir := t.TempDir()
	inputs := writeTones(t, dir, 3)
	inputs[1] = filepath.Join(dir, "missing.wav")
	r := NewRunner(RunnerConfig{})
	var l memberLedger
	r.openMember = l.open
	_, _, err := r.AnalyzeGroup(context.Background(), inputs, nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want the missing file's own error in the chain", err)
	}
	if _, ok := errors.AsType[*fs.PathError](err); !ok {
		t.Errorf("err = %T %v, want a *fs.PathError (exit 10, not bad input)", err, err)
	}
	if !strings.Contains(err.Error(), "group member 1") {
		t.Errorf("err = %q, want it to name group member 1", err)
	}
	if len(l.opened) != 0 {
		t.Errorf("decoded %v, want nothing: the refusal is found before the first decode", l.opened)
	}
	if l.held != 0 {
		t.Errorf("%d members still open after the failure", l.held)
	}
}

// A member that opens but cannot be read stops the run where the engine
// reaches it: the pre-flight above only asks the filesystem, so a file that
// is not audio is still the lazy open's refusal, under its own member index
// and with the members after it never opened.
func TestAnalyzeGroupStopsAtTheFirstUnreadableMember(t *testing.T) {
	dir := t.TempDir()
	inputs := writeTones(t, dir, 3)
	inputs[1] = writeFixture(t, dir, "junk.wav", []byte("not audio"))
	r := NewRunner(RunnerConfig{})
	var l memberLedger
	r.openMember = l.open
	_, _, err := r.AnalyzeGroup(context.Background(), inputs, nil)
	if err == nil {
		t.Fatal("a member that is not audio was measured")
	}
	if !strings.Contains(err.Error(), "group member 1") {
		t.Errorf("err = %q, want it to name group member 1", err)
	}
	if want := []string{"t0.wav"}; !slices.Equal(l.opened, want) {
		t.Errorf("opened %v, want %v: nothing after the failure", l.opened, want)
	}
	if l.held != 0 {
		t.Errorf("%d members still open after the failure", l.held)
	}
}

// The damage each member's read found comes back on that member's result, in
// the source's own terms, and the group's own result carries none.
func TestAnalyzeGroupCarriesMemberWarnings(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner(RunnerConfig{})
	clean := writeTones(t, dir, 1)[0]
	_, torn := tornADTS(t, r, dir)
	group, members, err := r.AnalyzeGroup(context.Background(), []string{clean, torn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ws := members[0].InputWarnings; ws != nil {
		t.Errorf("the clean member reports %v, want nil", ws)
	}
	if ws := members[1].InputWarnings; len(ws) == 0 {
		t.Errorf("the torn member reports no damage")
	} else if strings.HasPrefix(ws[0], "member ") {
		t.Errorf("the torn member's damage %q still carries a member prefix", ws[0])
	}
	if ws := group.InputWarnings; ws != nil {
		t.Errorf("the group reports %v, want nil: each member's damage is on its own result", ws)
	}
}
