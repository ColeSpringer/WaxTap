package waxtap

import (
	"fmt"
	"sync"

	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/internal/pipeline"
)

// emitter sends stage events to the caller and collects warnings for the Result.
// Callback panics are ignored.
//
// emitter is safe for concurrent use, and callbacks are serialized.
type emitter struct {
	fn      func(Event)
	videoID string

	cbMu sync.Mutex

	warnMu   sync.Mutex
	warnings []Warning
	seen     map[throttleKey]bool
}

func newEmitter(fn func(Event), videoID string) *emitter {
	return &emitter{fn: fn, videoID: videoID}
}

// raw sends one event, fills in the video ID, and ignores callback panics.
func (e *emitter) raw(ev Event) {
	if e.fn == nil {
		return
	}
	if ev.VideoID == "" {
		ev.VideoID = e.videoID
	}
	e.cbMu.Lock()
	defer e.cbMu.Unlock()
	defer func() { _ = recover() }()
	e.fn(ev)
}

func (e *emitter) stage(s Stage) { e.raw(Event{Stage: s}) }

func (e *emitter) progress(bytes, total int64) {
	e.raw(Event{Stage: StageDownloading, Bytes: bytes, Total: total})
}

// warn records a warning and emits it as a StageWarning event. The recorded
// warnings are copied into Result.Warnings when the job finishes.
func (e *emitter) warn(code WarningCode, detail string) {
	w := Warning{Code: code, Detail: detail}
	e.warnMu.Lock()
	e.warnings = append(e.warnings, w)
	e.warnMu.Unlock()
	e.raw(Event{Stage: StageWarning, Warning: &w})
}

func (e *emitter) done()            { e.raw(Event{Stage: StageDone}) }
func (e *emitter) failed(err error) { e.raw(Event{Stage: StageFailed, Err: err}) }

// throttleKey identifies a throttle warning for per-job deduplication.
type throttleKey struct {
	code   WarningCode
	host   string
	status int
}

// emitThrottle records at most one warning per code, host, and status.
func emitThrottle(e *emitter, ev httpx.ThrottleEvent) {
	code := WarnThrottled
	if ev.Phase == httpx.ThrottleRetryStarted {
		code = WarnRateLimitedRetried
	}
	key := throttleKey{code: code, host: ev.Host, status: ev.StatusCode}

	w := Warning{Code: code, Detail: throttleDetail(code, ev)}
	e.warnMu.Lock()
	if e.seen[key] {
		e.warnMu.Unlock()
		return
	}
	if e.seen == nil {
		e.seen = make(map[throttleKey]bool)
	}
	e.seen[key] = true
	e.warnings = append(e.warnings, w)
	e.warnMu.Unlock()
	e.raw(Event{Stage: StageWarning, Warning: &w})
}

// throttleDetail formats a throttle warning.
func throttleDetail(code WarningCode, ev httpx.ThrottleEvent) string {
	if code == WarnRateLimitedRetried {
		return fmt.Sprintf("retrying request to %s after HTTP %d", ev.Host, ev.StatusCode)
	}
	if ev.Penalty > 0 {
		return fmt.Sprintf("rate limited by %s (HTTP %d); pausing %s", ev.Host, ev.StatusCode, ev.Penalty)
	}
	return fmt.Sprintf("rate limited by %s (HTTP %d)", ev.Host, ev.StatusCode)
}

// pipelineStage forwards a pipeline stage as the matching public Stage.
func (e *emitter) pipelineStage(s pipeline.Stage) { e.stage(mapPipelineStage(s)) }

// finish copies accumulated warnings into res (when non-nil) and fires the
// terminal event: StageDone on success or StageFailed carrying err. It is meant
// to be deferred so the terminal event fires after any cleanup.
func (e *emitter) finish(res *Result, err error) {
	if res != nil {
		e.warnMu.Lock()
		res.Warnings = append(res.Warnings, e.warnings...)
		e.warnMu.Unlock()
	}
	if err != nil {
		e.failed(err)
		return
	}
	e.done()
}

// mapPipelineStage maps an internal pipeline stage onto the public Stage.
func mapPipelineStage(s pipeline.Stage) Stage {
	switch s {
	case pipeline.StageProbing:
		return StageProbing
	case pipeline.StageAnalyzing:
		return StageAnalyzing
	case pipeline.StageCutting:
		return StageCutting
	case pipeline.StageNormalizing:
		return StageNormalizing
	case pipeline.StageTranscoding:
		return StageTranscoding
	default:
		return StageProbing
	}
}
