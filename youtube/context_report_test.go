package youtube

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/colespringer/waxtap/v3/potoken"
)

// stubContextProvider is a player-context provider that records the
// invalidations it is asked for. failNext makes the next invalidation fail, the
// way a rate-limited or unreachable minter would.
type stubContextProvider struct {
	mu       sync.Mutex
	reports  []potoken.SessionInvalidation
	failNext error
}

func (p *stubContextProvider) ProvidePlayerContext(context.Context, string) (potoken.PlayerContext, error) {
	return potoken.PlayerContext{}, errors.New("not under test")
}

func (p *stubContextProvider) InvalidateSession(_ context.Context, inv potoken.SessionInvalidation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failNext; err != nil {
		p.failNext = nil
		return err
	}
	p.reports = append(p.reports, inv)
	return nil
}

// plainContextProvider supplies contexts but cannot retire their sessions.
type plainContextProvider struct{}

func (plainContextProvider) ProvidePlayerContext(context.Context, string) (potoken.PlayerContext, error) {
	return potoken.PlayerContext{}, errors.New("not under test")
}

// TestReportPlayerContext covers the report path: the provider receives the
// generation, video, and reason, and repeat reports for one generation collapse
// into a single call.
func TestReportPlayerContext(t *testing.T) {
	p := &stubContextProvider{}
	c := New(Config{PlayerContextProvider: p})

	if !c.ReportPlayerContext(context.Background(), 7, "testVideo01") {
		t.Fatal("ReportPlayerContext = false, want true for an invalidating provider")
	}
	if !c.ReportPlayerContext(context.Background(), 7, "testVideo01") {
		t.Fatal("a repeat report for the same generation must still report success")
	}
	if len(p.reports) != 1 {
		t.Fatalf("invalidations = %d, want 1 (same-generation reports must collapse)", len(p.reports))
	}
	if got := p.reports[0]; got.Generation != 7 || got.VideoID != "testVideo01" || got.Reason != invalidationReasonStreamCapped {
		t.Errorf("invalidation = %+v, want generation 7 for testVideo01 with reason %q", got, invalidationReasonStreamCapped)
	}

	// A later generation is a new session and reports again.
	if !c.ReportPlayerContext(context.Background(), 8, "testVideo01") {
		t.Fatal("a new generation must report")
	}
	if len(p.reports) != 2 {
		t.Errorf("invalidations = %d, want 2", len(p.reports))
	}
}

// TestReportPlayerContext_Refusals pins the no-report cases: a provider without
// the invalidator, and a context that carried no generation.
func TestReportPlayerContext_Refusals(t *testing.T) {
	c := New(Config{PlayerContextProvider: plainContextProvider{}})
	if c.ReportPlayerContext(context.Background(), 7, "") {
		t.Error("ReportPlayerContext = true for a provider that cannot invalidate")
	}

	p := &stubContextProvider{}
	c = New(Config{PlayerContextProvider: p})
	if c.ReportPlayerContext(context.Background(), 0, "") {
		t.Error("ReportPlayerContext = true for an unversioned context (generation 0)")
	}
	if len(p.reports) != 0 {
		t.Errorf("invalidations = %d, want 0", len(p.reports))
	}
}

// TestReportPlayerContext_FailureRetries pins that a rejected report is not
// cached: a later cap on the same generation tries again.
func TestReportPlayerContext_FailureRetries(t *testing.T) {
	p := &stubContextProvider{failNext: errors.New("recycling is rate-limited")}
	c := New(Config{PlayerContextProvider: p})

	if c.ReportPlayerContext(context.Background(), 7, "") {
		t.Fatal("ReportPlayerContext = true, want false when the provider rejects the report")
	}
	if !c.ReportPlayerContext(context.Background(), 7, "") {
		t.Fatal("a rejected report must be retryable for the same generation")
	}
	if len(p.reports) != 1 {
		t.Errorf("accepted invalidations = %d, want 1", len(p.reports))
	}
}
