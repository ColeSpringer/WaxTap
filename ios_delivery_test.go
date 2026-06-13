package waxtap

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/colespringer/waxtap/potoken"
)

// TestIOSDeliveryGuard verifies that forced iOS blocks byte delivery without
// blocking metadata extraction or an earlier WEB player-context delivery.
func TestIOSDeliveryGuard(t *testing.T) {
	t.Run("forced ios blocks delivery", func(t *testing.T) {
		c, err := New(Options{Client: "ios"})
		if err != nil {
			t.Fatal(err)
		}
		if gerr := c.forcedIOSDelivery(); !errors.Is(gerr, ErrDeliveryUnsupported) {
			t.Errorf("forcedIOSDelivery = %v, want ErrDeliveryUnsupported", gerr)
		}
		// End-to-end: Download rejects before any network work.
		_, derr := c.Download(context.Background(), Request{URL: "dummyVideo0", ProcessSpec: ProcessSpec{Output: ToFile(t.TempDir() + "/out.webm")}})
		if !errors.Is(derr, ErrDeliveryUnsupported) {
			t.Errorf("Download err = %v, want ErrDeliveryUnsupported", derr)
		}
		// Stream is guarded the same way.
		_, _, serr := c.Stream(context.Background(), Request{URL: "dummyVideo0"})
		if !errors.Is(serr, ErrDeliveryUnsupported) {
			t.Errorf("Stream err = %v, want ErrDeliveryUnsupported", serr)
		}
	})

	t.Run("forced ios with player-context still blocks the iOS byte path", func(t *testing.T) {
		// A player-context may deliver first, but the iOS byte path remains blocked.
		pc := potoken.PlayerContextProviderFunc(
			func(context.Context, string) (potoken.PlayerContext, error) { return potoken.PlayerContext{}, nil })
		c, err := New(Options{Client: "ios", PlayerContextProvider: pc, POTokenProvider: stubPOTokenProvider{}})
		if err != nil {
			t.Fatal(err)
		}
		if gerr := c.forcedIOSDelivery(); !errors.Is(gerr, ErrDeliveryUnsupported) {
			t.Errorf("forcedIOSDelivery = %v, want ErrDeliveryUnsupported (the iOS byte path is never usable)", gerr)
		}
	})

	t.Run("skip-existing short-circuits before the iOS block", func(t *testing.T) {
		c, err := New(Options{Client: "ios"})
		if err != nil {
			t.Fatal(err)
		}
		out := t.TempDir() + "/out.webm"
		if f, werr := os.Create(out); werr != nil {
			t.Fatal(werr)
		} else {
			f.Close()
		}
		res, derr := c.Download(context.Background(), Request{
			URL:         "dummyVideo0",
			ProcessSpec: ProcessSpec{Output: ToFile(out), SkipIfExists: true},
		})
		if derr != nil {
			t.Fatalf("SkipIfExists with an existing file should skip, not block: %v", derr)
		}
		if res == nil || res.OutputPath != out {
			t.Errorf("res = %+v, want a skipped Result for the existing output", res)
		}
	})

	t.Run("other forced clients and the default chain are not blocked", func(t *testing.T) {
		for _, client := range []string{"", "android_vr", "web", "web_embedded"} {
			c, err := New(Options{Client: client})
			if err != nil {
				t.Fatalf("New(Client=%q): %v", client, err)
			}
			if gerr := c.forcedIOSDelivery(); gerr != nil {
				t.Errorf("forcedIOSDelivery(Client=%q) = %v, want nil", client, gerr)
			}
		}
	})
}
