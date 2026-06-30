package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"text/tabwriter"
)

// batchCounts contains the totals reported after a batch.
type batchCounts struct {
	processed int
	copied    int
	unchanged int
	skipped   int
	ignored   int
	failed    int
	notRun    int
}

func countBatch(outcomes []batchOutcome, ignored int) batchCounts {
	c := batchCounts{ignored: ignored}
	for _, o := range outcomes {
		switch o.status {
		case statusOK:
			c.processed++
		case statusCopied:
			c.copied++
		case statusUnchanged:
			c.unchanged++
		case statusSkipped:
			c.skipped++
		case statusError:
			c.failed++
		case statusNotRun:
			c.notRun++
		}
	}
	return c
}

// batchItemRecord is the NDJSON representation of one batch item.
type batchItemRecord struct {
	SchemaVersion  int           `json:"schemaVersion"`
	Type           string        `json:"type"`
	Index          int           `json:"index"`
	Input          string        `json:"input"`
	Output         string        `json:"output,omitempty"`
	Status         string        `json:"status"`
	IntegratedLUFS *jsonFloat    `json:"integratedLufs,omitempty"`
	Error          string        `json:"error,omitempty"`
	Warnings       []warningJSON `json:"warnings,omitempty"`
}

// itemRecord builds an item record. Measure records contain loudness instead of
// an output path.
func itemRecord(o batchOutcome, measure bool) batchItemRecord {
	rec := batchItemRecord{SchemaVersion: schemaVersion, Type: "item", Index: o.index + 1, Input: o.input, Status: o.status.String()}
	if o.err != nil {
		rec.Error = friendlyError(o.err)
	}
	if measure {
		if o.result != nil && o.result.Loudness != nil && o.result.Loudness.Input != nil {
			lufs := jsonFloat(o.result.Loudness.Input.IntegratedLUFS)
			rec.IntegratedLUFS = &lufs
		}
	} else if o.status != statusUnchanged {
		rec.Output = o.output
	}
	if o.result != nil {
		for _, w := range o.result.Warnings {
			rec.Warnings = append(rec.Warnings, warningJSON{Code: w.Code.String(), Detail: w.Detail})
		}
	}
	return rec
}

// emitBatchJSON writes the NDJSON item records and final summary. Measurement
// records report loudness instead of an output path.
func emitBatchJSON(env *appEnv, outcomes []batchOutcome, ignored int, measure bool) {
	for _, o := range outcomes {
		writeBatchJSON(env, itemRecord(o, measure))
	}
	emitBatchSummaryJSON(env, countBatch(outcomes, ignored))
}

// emitBatchProcess writes per-file results and a summary. JSON mode uses NDJSON.
func emitBatchProcess(env *appEnv, outcomes []batchOutcome, ignored int, fmtName string) {
	if env.jsonMode() {
		emitBatchJSON(env, outcomes, ignored, false)
		return
	}
	for _, o := range outcomes {
		switch o.status {
		case statusOK:
			env.printf("ok:        %s -> %s\n", o.input, o.output)
		case statusCopied:
			env.printf("copied:    %s -> %s (already %s)\n", o.input, o.output, fmtName)
		case statusUnchanged:
			env.printf("unchanged: %s\n", o.input)
		case statusSkipped:
			env.printf("skip:      %s (exists)\n", o.output)
		case statusNotRun:
			env.printf("not run:   %s\n", o.input)
		case statusError:
			env.printf("FAIL:      %s: %s\n", o.input, friendlyError(o.err))
		}
	}
	emitBatchSummaryHuman(env, countBatch(outcomes, ignored), "encoded")
}

// emitBatchMeasure writes a loudness table or NDJSON item records and a summary.
func emitBatchMeasure(env *appEnv, outcomes []batchOutcome, ignored int) {
	if env.jsonMode() {
		emitBatchJSON(env, outcomes, ignored, true)
		return
	}
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tLUFS\tFILE")
	for _, o := range outcomes {
		if o.status != statusOK {
			continue
		}
		lufs := "n/a"
		if o.result != nil && o.result.Loudness != nil && o.result.Loudness.Input != nil {
			lufs = humanLUFS(o.result.Loudness.Input.IntegratedLUFS)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\n", o.index+1, lufs, filepath.Base(o.input))
	}
	tw.Flush()
	for _, o := range outcomes {
		if o.status == statusError {
			env.printf("FAIL: %s: %s\n", o.input, friendlyError(o.err))
		}
	}
	emitBatchSummaryHuman(env, countBatch(outcomes, ignored), "measured")
}

// emitBatchSummaryHuman prints one summary line. The leading total includes every
// terminal state; c.processed names only files that were encoded or measured.
// verb is the word shown for statusOK entries. Zero-value optional states are
// omitted.
func emitBatchSummaryHuman(env *appEnv, c batchCounts, verb string) {
	total := c.processed + c.copied + c.unchanged + c.ignored + c.failed + c.skipped + c.notRun
	line := fmt.Sprintf("%d files: %d %s, %d copied, %d unchanged, %d failed",
		total, c.processed, verb, c.copied, c.unchanged, c.failed)
	if c.ignored > 0 {
		line += fmt.Sprintf(", %d ignored", c.ignored)
	}
	if c.skipped > 0 {
		line += fmt.Sprintf(", %d skipped", c.skipped)
	}
	if c.notRun > 0 {
		line += fmt.Sprintf(", %d not run", c.notRun)
	}
	env.printf("%s\n", line)
}

// emitBatchSummaryJSON writes the final summary NDJSON record.
func emitBatchSummaryJSON(env *appEnv, c batchCounts) {
	writeBatchJSON(env, struct {
		SchemaVersion int    `json:"schemaVersion"`
		Type          string `json:"type"`
		Processed     int    `json:"processed"`
		Copied        int    `json:"copied,omitempty"`
		Unchanged     int    `json:"unchanged,omitempty"`
		Skipped       int    `json:"skipped,omitempty"`
		Ignored       int    `json:"ignored,omitempty"`
		Failed        int    `json:"failed,omitempty"`
		NotRun        int    `json:"notRun,omitempty"`
	}{schemaVersion, "summary", c.processed, c.copied, c.unchanged, c.skipped, c.ignored, c.failed, c.notRun})
}

// writeBatchJSON writes one compact NDJSON record followed by a newline.
func writeBatchJSON(env *appEnv, rec any) {
	if b, err := json.Marshal(rec); err == nil {
		fmt.Fprintf(env.out, "%s\n", b)
	}
}

// batchProgress returns a per-item progress reporter that writes to stderr and
// honors --quiet, leaving stdout for final results.
func batchProgress(env *appEnv, total int) func(batchOutcome) {
	done := 0
	return func(o batchOutcome) {
		done++
		env.info("[%d/%d] %s (%s)\n", done, total, filepath.Base(o.input), o.status)
	}
}
