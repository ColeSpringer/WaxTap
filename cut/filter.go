package cut

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// Graph builds an ffmpeg -filter_complex graph that trims [0:a:0] to the keep
// ranges and joins the pieces at [sink]. Callers either map [sink] or continue
// the graph from it, such as appending loudnorm before a final [out].
//
// total is the true media duration: a keep span that reaches it drops its atrim
// end bound and runs cleanly to EOF instead of relying on a rounded end time.
//
// Each kept span is isolated with atrim and rebased to a zero start with
// asetpts. When crossfade > 0 and more than one span remains, adjacent spans are
// joined with acrossfade instead of concat. Graph returns "" when there is
// nothing to keep.
func Graph(keeps []Range, crossfade, total time.Duration, sink string) string {
	switch len(keeps) {
	case 0:
		return ""
	case 1:
		// One span: trim straight to the sink, no concat/crossfade needed.
		return trimChain(keeps[0], total, "[0:a:0]", brackets(sink))
	}

	var b strings.Builder
	labels := make([]string, len(keeps))
	for i, k := range keeps {
		labels[i] = fmt.Sprintf("[s%d]", i)
		b.WriteString(trimChain(k, total, "[0:a:0]", labels[i]))
		b.WriteByte(';')
	}

	if crossfade > 0 {
		writeCrossfade(&b, labels, crossfade, brackets(sink))
	} else {
		writeConcat(&b, labels, brackets(sink))
	}
	return b.String()
}

// encodeGraph builds the full accurate-cut graph. Linear encode filters run
// after the cut in the same -filter_complex graph; without filters the cut ends
// directly at [out].
func encodeGraph(keeps []Range, crossfade, total time.Duration, filters []string) string {
	if len(filters) == 0 {
		return Graph(keeps, crossfade, total, "out")
	}
	return Graph(keeps, crossfade, total, "cut") + ";[cut]" + strings.Join(filters, ",") + "[out]"
}

// ValidateCrossfade checks whether the retained spans can supply the requested
// overlap. acrossfade consumes d from both sides of each join, so an interior
// span must be at least 2*d. Rejecting short spans before ffmpeg avoids a
// successful command that emits no audio.
func ValidateCrossfade(keeps []Range, d time.Duration) error {
	if d <= 0 || len(keeps) < 2 {
		return nil // no acrossfade is emitted
	}
	for i, k := range keeps {
		required := d
		if i != 0 && i != len(keeps)-1 {
			required = 2 * d // interior spans fade on both ends
		}
		if k.Duration() < required {
			return fmt.Errorf("%w: crossfade %v is too long for the %v span kept at %v (needs %v)",
				waxerr.ErrIncompatibleSpec, d, k.Duration(), k.Start, required)
		}
	}
	return nil
}

// trimChain writes "<in>atrim=...,asetpts=PTS-STARTPTS<out>" for one keep span.
// The end bound is omitted when the span reaches the media end, avoiding a
// rounded end time at EOF.
func trimChain(k Range, total time.Duration, in, out string) string {
	atrim := "atrim=start=" + secs(k.Start)
	if k.End < total {
		atrim += ":end=" + secs(k.End)
	}
	return in + atrim + ",asetpts=PTS-STARTPTS" + out
}

// writeConcat appends "[s0][s1]...concat=n=N:v=0:a=1<out>" to b.
func writeConcat(b *strings.Builder, labels []string, out string) {
	for _, l := range labels {
		b.WriteString(l)
	}
	fmt.Fprintf(b, "concat=n=%d:v=0:a=1%s", len(labels), out)
}

// writeCrossfade appends a left-folded chain of acrossfade joins to b: each join
// blends the running result with the next span over d seconds. With N spans it
// emits N-1 joins, the last writing to out.
func writeCrossfade(b *strings.Builder, labels []string, d time.Duration, out string) {
	prev := labels[0]
	for i := 1; i < len(labels); i++ {
		dst := out
		if i < len(labels)-1 {
			dst = fmt.Sprintf("[x%d]", i)
		}
		fmt.Fprintf(b, "%s%sacrossfade=d=%s%s", prev, labels[i], secs(d), dst)
		if i < len(labels)-1 {
			b.WriteByte(';')
		}
		prev = dst
	}
}

func brackets(label string) string { return "[" + label + "]" }

// secs formats a duration as seconds with microsecond precision, enough for
// sample-accurate trims at any common rate.
func secs(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 6, 64)
}
