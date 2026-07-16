package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/colespringer/waxtap/v3"
)

// progressReporter renders a single download's stage and byte progress. On a
// TTY it draws in place; off a TTY it prints only stage transitions.
//
// It is intended for a single (non-parallel) download. Playlist runs use a
// quieter per-item completion printer to avoid interleaved bars.
type progressReporter struct {
	w       io.Writer
	enabled bool
	tty     bool

	mu       sync.Mutex
	lastLen  int
	lastStg  waxtap.Stage
	haveStg  bool
	finished bool
}

func (e *appEnv) newProgress() *progressReporter {
	return &progressReporter{
		w:       e.errOut,
		enabled: !e.jsonMode() && !e.quiet(),
		tty:     isTerminal(e.errOut),
	}
}

// handle is the waxtap Events callback. It is invoked synchronously from the
// worker, so it must stay fast.
func (r *progressReporter) handle(ev waxtap.Event) {
	if !r.enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}

	switch ev.Stage {
	case waxtap.StageDownloading:
		r.renderDownloading(ev)
	case waxtap.StageWarning:
		if ev.Warning != nil {
			// Non-quiet runs show warnings only here, so include the stable code.
			r.printAbove(fmt.Sprintf("warning: [%s] %s", ev.Warning.Code, ev.Warning.Detail))
		}
	case waxtap.StageDone, waxtap.StageFailed:
		r.clearLocked()
	default:
		r.renderStage(ev.Stage)
	}
}

// finish clears any in-place line. Safe to call more than once.
func (r *progressReporter) finish() {
	if !r.enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
	r.finished = true
}

func (r *progressReporter) renderDownloading(ev waxtap.Event) {
	// Non-TTY output is line-oriented: announce the stage once and leave byte
	// counts to the final summary. The transition event carries no progress data,
	// and small downloads can finish before a progress snapshot arrives.
	if !r.tty {
		r.renderStage(waxtap.StageDownloading)
		return
	}
	var line string
	if ev.Total > 0 {
		frac := float64(ev.Bytes) / float64(ev.Total)
		line = fmt.Sprintf("downloading %s %3.0f%% %s/%s",
			bar(frac, 24), frac*100, humanBytes(ev.Bytes), humanBytes(ev.Total))
	} else {
		line = fmt.Sprintf("downloading %s", humanBytes(ev.Bytes))
	}
	r.writeLineLocked(line)
}

func (r *progressReporter) renderStage(s waxtap.Stage) {
	if r.haveStg && r.lastStg == s {
		return
	}
	r.lastStg, r.haveStg = s, true
	label := s.String()
	if r.tty {
		r.writeLineLocked(label)
		return
	}
	fmt.Fprintln(r.w, label)
}

// printAbove emits a standalone line (e.g. a warning), preserving the in-place
// progress line beneath it on a TTY.
func (r *progressReporter) printAbove(msg string) {
	if r.tty {
		r.clearLocked()
	}
	fmt.Fprintln(r.w, msg)
	r.haveStg = false // force the next stage line to redraw
}

// writeLineLocked draws s in place, padding to erase any longer previous line.
func (r *progressReporter) writeLineLocked(s string) {
	r.lastLen = overwriteLine(r.w, s, r.lastLen)
}

// overwriteLine writes s after a carriage return and pads with spaces to erase a
// longer previous line. It returns len(s) for the next overwrite.
func overwriteLine(w io.Writer, s string, prevLen int) int {
	pad := ""
	if d := prevLen - len(s); d > 0 {
		pad = strings.Repeat(" ", d)
	}
	fmt.Fprintf(w, "\r%s%s", s, pad)
	return len(s)
}

// clearLocked erases the current in-place line on a TTY.
func (r *progressReporter) clearLocked() {
	if r.tty && r.lastLen > 0 {
		fmt.Fprintf(r.w, "\r%s\r", strings.Repeat(" ", r.lastLen))
	}
	r.lastLen = 0
}

// listHeartbeat owns the transient status line used by download --list while
// enumeration and enrichment are running. Enrichment callbacks can run from
// multiple goroutines, so writes share one lock.
type listHeartbeat struct {
	w       io.Writer
	mu      sync.Mutex
	lastLen int
}

// write updates the transient line. A nil receiver disables the heartbeat.
func (h *listHeartbeat) write(s string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastLen = overwriteLine(h.w, s, h.lastLen)
}

// finish ends the transient line with a newline. It is safe to call on nil or
// after the line has already been finished.
func (h *listHeartbeat) finish() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastLen > 0 {
		fmt.Fprint(h.w, "\n")
		h.lastLen = 0
	}
}

// bar renders a [###---] progress bar of the given inner width.
func bar(frac float64, width int) string {
	switch {
	case frac < 0:
		frac = 0
	case frac > 1:
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

// isTerminal reports whether w is a character device (a terminal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
