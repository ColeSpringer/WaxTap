package waxtap

import "github.com/colespringer/waxtap/internal/pipeline"

// emitter delivers stage events to a caller-supplied callback and accumulates
// warnings for the Result. Callbacks are best-effort: each invocation is
// panic-recovered so a misbehaving callback cannot crash a job, and a nil
// callback is a no-op. Warnings are both emitted as StageWarning events and kept
// so the facade can copy them into Result.Warnings.
type emitter struct {
	fn       func(Event)
	videoID  string
	warnings []Warning
}

func newEmitter(fn func(Event), videoID string) *emitter {
	return &emitter{fn: fn, videoID: videoID}
}

// raw delivers one event, stamping the video ID and recovering any panic from the
// callback.
func (e *emitter) raw(ev Event) {
	if e.fn == nil {
		return
	}
	if ev.VideoID == "" {
		ev.VideoID = e.videoID
	}
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
	e.warnings = append(e.warnings, w)
	e.raw(Event{Stage: StageWarning, Warning: &w})
}

func (e *emitter) done()            { e.raw(Event{Stage: StageDone}) }
func (e *emitter) failed(err error) { e.raw(Event{Stage: StageFailed, Err: err}) }

// pipelineStage forwards a pipeline stage as the matching public Stage.
func (e *emitter) pipelineStage(s pipeline.Stage) { e.stage(mapPipelineStage(s)) }

// finish copies accumulated warnings into res (when non-nil) and fires the
// terminal event: StageDone on success or StageFailed carrying err. It is meant
// to be deferred so the terminal event fires after any cleanup.
func (e *emitter) finish(res *Result, err error) {
	if res != nil {
		res.Warnings = append(res.Warnings, e.warnings...)
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
