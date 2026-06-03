package cut

import (
	"strings"
	"testing"
	"time"
)

func TestGraph_Empty(t *testing.T) {
	if got := Graph(nil, 0, s(100), "out"); got != "" {
		t.Errorf("Graph(no keeps) = %q, want empty", got)
	}
}

func TestGraph_SingleSpanToEOF(t *testing.T) {
	// One span that reaches the media end: no concat, end bound omitted.
	got := Graph([]Range{{s(10), s(100)}}, 0, s(100), "out")
	want := "[0:a:0]atrim=start=10.000000,asetpts=PTS-STARTPTS[out]"
	if got != want {
		t.Errorf("Graph =\n  %q\nwant\n  %q", got, want)
	}
}

func TestGraph_SingleSpanBounded(t *testing.T) {
	// One span shorter than the media: both bounds present.
	got := Graph([]Range{{s(10), s(40)}}, 0, s(100), "out")
	want := "[0:a:0]atrim=start=10.000000:end=40.000000,asetpts=PTS-STARTPTS[out]"
	if got != want {
		t.Errorf("Graph =\n  %q\nwant\n  %q", got, want)
	}
}

func TestGraph_MultiSpanConcat(t *testing.T) {
	// Keep [0,10) and [20,100): two atrim chains then a concat. The first span is
	// bounded; the last reaches EOF so its end bound is dropped.
	got := Graph([]Range{{0, s(10)}, {s(20), s(100)}}, 0, s(100), "out")
	want := "[0:a:0]atrim=start=0.000000:end=10.000000,asetpts=PTS-STARTPTS[s0];" +
		"[0:a:0]atrim=start=20.000000,asetpts=PTS-STARTPTS[s1];" +
		"[s0][s1]concat=n=2:v=0:a=1[out]"
	if got != want {
		t.Errorf("Graph =\n  %q\nwant\n  %q", got, want)
	}
}

func TestGraph_Crossfade(t *testing.T) {
	// Two spans with a crossfade: a single acrossfade join into [out].
	got := Graph([]Range{{0, s(10)}, {s(20), s(40)}}, 2*time.Second, s(100), "out")
	want := "[0:a:0]atrim=start=0.000000:end=10.000000,asetpts=PTS-STARTPTS[s0];" +
		"[0:a:0]atrim=start=20.000000:end=40.000000,asetpts=PTS-STARTPTS[s1];" +
		"[s0][s1]acrossfade=d=2.000000[out]"
	if got != want {
		t.Errorf("Graph =\n  %q\nwant\n  %q", got, want)
	}
}

func TestGraph_CrossfadeChained(t *testing.T) {
	// Three spans crossfaded: two joins, an intermediate [x1] then [out].
	got := Graph([]Range{{0, s(10)}, {s(20), s(30)}, {s(40), s(50)}}, time.Second, s(100), "out")
	// Two acrossfade chains after the three trims.
	joins := got[strings.LastIndex(got, "[s2];")+len("[s2];"):]
	want := "[s0][s1]acrossfade=d=1.000000[x1];[x1][s2]acrossfade=d=1.000000[out]"
	if joins != want {
		t.Errorf("crossfade joins =\n  %q\nwant\n  %q", joins, want)
	}
}

func TestGraph_CustomSink(t *testing.T) {
	// The sink label lets the pipeline extend the graph (e.g. append loudnorm).
	got := Graph([]Range{{0, s(10)}, {s(20), s(100)}}, 0, s(100), "cut")
	if !strings.HasSuffix(got, "concat=n=2:v=0:a=1[cut]") {
		t.Errorf("Graph should end at [cut], got %q", got)
	}
}
