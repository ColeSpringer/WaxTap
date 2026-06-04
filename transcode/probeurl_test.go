package transcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestFormatProbeHeaders(t *testing.T) {
	if got := formatProbeHeaders(nil); got != "" {
		t.Errorf("nil headers = %q, want empty", got)
	}
	h := http.Header{}
	h.Set("User-Agent", "WaxTap/1.0")
	if got := formatProbeHeaders(h); got != "User-Agent: WaxTap/1.0\r\n" {
		t.Errorf("formatProbeHeaders = %q", got)
	}
}

// TestProbeURLSendsHeaders proves -headers are passed to ffprobe by serving a
// synthesized file over HTTP and asserting the custom header arrived.
func TestProbeURLSendsHeaders(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	file := synthSine(t, dir, 1) // dir/in.wav

	var mu sync.Mutex
	saw := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Waxtap-Probe") == "yes" {
			mu.Lock()
			saw = true
			mu.Unlock()
		}
		http.ServeFile(w, req, file)
	}))
	defer srv.Close()

	h := http.Header{}
	h.Set("X-Waxtap-Probe", "yes")
	pr, err := r.ProbeURL(context.Background(), srv.URL, h)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if _, ok := pr.AudioStream(); !ok {
		t.Error("ProbeURL returned no audio stream")
	}
	mu.Lock()
	defer mu.Unlock()
	if !saw {
		t.Error("server did not receive the custom probe header")
	}
}
