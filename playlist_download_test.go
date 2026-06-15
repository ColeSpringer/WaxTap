package waxtap

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeEntries(n int) []PlaylistEntry {
	es := make([]PlaylistEntry, n)
	for i := range es {
		es[i] = PlaylistEntry{VideoID: fmt.Sprintf("dummyVideo%d", i), Index: i}
	}
	return es
}

func noWait(context.Context, int) error { return nil }

func okBuildRequest(_ context.Context, e PlaylistEntry) (Request, string, error) {
	return Request{URL: e.VideoID}, "", nil
}

func checkInvariant(t *testing.T, res *PlaylistRunResult) {
	t.Helper()
	sum := res.Downloaded + res.Skipped + res.BuildRequestFailed + res.DownloadFailed + res.Remaining
	if sum != res.Enumerated {
		t.Errorf("invariant: downloaded=%d skipped=%d buildRequestFailed=%d downloadFailed=%d remaining=%d total=%d, want enumerated=%d",
			res.Downloaded, res.Skipped, res.BuildRequestFailed, res.DownloadFailed, res.Remaining, sum, res.Enumerated)
	}
	if got, want := len(res.Outcomes), res.Enumerated-res.Remaining; got != want {
		t.Errorf("len(Outcomes) = %d, want Enumerated-Remaining = %d", got, want)
	}
}

func TestRunPlaylist_MaxDownloadsCap(t *testing.T) {
	entries := makeEntries(10)
	var attempts atomic.Int32
	dl := func(context.Context, Request) (*Result, error) {
		attempts.Add(1)
		return &Result{}, nil
	}
	res := runPlaylist(context.Background(), entries, 2, 3, noWait, okBuildRequest, nil, dl)

	if got := attempts.Load(); got != 3 {
		t.Errorf("download attempts = %d, want 3 (the cap)", got)
	}
	if res.Downloaded != 3 {
		t.Errorf("Downloaded = %d, want 3", res.Downloaded)
	}
	if !res.CapReached {
		t.Error("CapReached = false, want true")
	}
	if res.Remaining != 7 {
		t.Errorf("Remaining = %d, want 7", res.Remaining)
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_OutcomeSplit(t *testing.T) {
	entries := makeEntries(6)
	buildRequest := func(_ context.Context, e PlaylistEntry) (Request, string, error) {
		switch e.Index {
		case 0:
			return Request{}, "exists", nil
		case 1:
			return Request{}, "", errors.New("build request failed")
		default:
			return Request{URL: e.VideoID}, "", nil
		}
	}
	dl := func(_ context.Context, req Request) (*Result, error) {
		if req.URL == "dummyVideo3" {
			return nil, errors.New("download failed")
		}
		return &Result{OutputPath: req.URL}, nil
	}
	res := runPlaylist(context.Background(), entries, 3, 0, noWait, buildRequest, nil, dl)

	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
	if res.BuildRequestFailed != 1 {
		t.Errorf("BuildRequestFailed = %d, want 1", res.BuildRequestFailed)
	}
	if res.DownloadFailed != 1 {
		t.Errorf("DownloadFailed = %d, want 1", res.DownloadFailed)
	}
	if res.Downloaded != 3 {
		t.Errorf("Downloaded = %d, want 3", res.Downloaded)
	}
	if res.Remaining != 0 || res.CapReached {
		t.Errorf("Remaining = %d, CapReached = %v, want 0/false", res.Remaining, res.CapReached)
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_BuildRequestErrorsDoNotConsumeCap(t *testing.T) {
	entries := makeEntries(6)
	buildRequest := func(_ context.Context, e PlaylistEntry) (Request, string, error) {
		if e.Index < 3 {
			return Request{}, "", errors.New("build request failed")
		}
		return Request{URL: e.VideoID}, "", nil
	}
	var attempts atomic.Int32
	dl := func(context.Context, Request) (*Result, error) {
		attempts.Add(1)
		return &Result{}, nil
	}
	res := runPlaylist(context.Background(), entries, 2, 2, noWait, buildRequest, nil, dl)

	// BuildRequest errors do not consume the two-attempt limit.
	if got := attempts.Load(); got != 2 {
		t.Errorf("download attempts = %d, want 2", got)
	}
	if res.BuildRequestFailed != 3 {
		t.Errorf("BuildRequestFailed = %d, want 3", res.BuildRequestFailed)
	}
	if res.Downloaded != 2 {
		t.Errorf("Downloaded = %d, want 2", res.Downloaded)
	}
	if !res.CapReached || res.Remaining != 1 {
		t.Errorf("CapReached = %v, Remaining = %d, want true/1", res.CapReached, res.Remaining)
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_OutcomesInPlaylistOrder(t *testing.T) {
	entries := makeEntries(8)
	dl := func(_ context.Context, req Request) (*Result, error) {
		// Make completion order the reverse of playlist order.
		var idx int
		fmt.Sscanf(req.URL, "dummyVideo%d", &idx)
		time.Sleep(time.Duration(len(entries)-idx) * time.Millisecond)
		return &Result{OutputPath: req.URL}, nil
	}
	res := runPlaylist(context.Background(), entries, len(entries), 0, noWait, okBuildRequest, nil, dl)

	if len(res.Outcomes) != len(entries) {
		t.Fatalf("len(Outcomes) = %d, want %d", len(res.Outcomes), len(entries))
	}
	for i, o := range res.Outcomes {
		if o.Entry.Index != i {
			t.Errorf("Outcomes[%d].Entry.Index = %d, want %d (not playlist order)", i, o.Entry.Index, i)
		}
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_CancelDuringBuildRequestLeavesRemaining(t *testing.T) {
	entries := makeEntries(5)
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	buildRequest := func(_ context.Context, e PlaylistEntry) (Request, string, error) {
		if e.Index == 2 {
			cancel() // cancel while building the third entry's request
		}
		return Request{URL: e.VideoID}, "", nil
	}
	dl := func(context.Context, Request) (*Result, error) {
		attempts.Add(1)
		return &Result{}, nil
	}
	res := runPlaylist(ctx, entries, 2, 0, noWait, buildRequest, nil, dl)

	if got := attempts.Load(); got != 2 {
		t.Errorf("download attempts = %d, want 2 (entries 0,1)", got)
	}
	if res.DownloadFailed != 0 || res.BuildRequestFailed != 0 {
		t.Errorf("DownloadFailed = %d, BuildRequestFailed = %d, want 0/0 (canceled build request is Remaining)", res.DownloadFailed, res.BuildRequestFailed)
	}
	if res.Remaining != 3 {
		t.Errorf("Remaining = %d, want 3 (entries 2,3,4)", res.Remaining)
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_CancelDuringWaitLeavesRemaining(t *testing.T) {
	entries := makeEntries(5)
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	dl := func(context.Context, Request) (*Result, error) {
		attempts.Add(1)
		return &Result{}, nil
	}
	wait := func(_ context.Context, started int) error {
		if started == 1 { // the first attempt dispatched; cancel the second's wait
			cancel()
			return context.Canceled
		}
		return nil
	}
	res := runPlaylist(ctx, entries, 2, 0, wait, okBuildRequest, nil, dl)

	if got := attempts.Load(); got != 1 {
		t.Errorf("download attempts = %d, want 1 (wait canceled before the second)", got)
	}
	if res.Downloaded != 1 {
		t.Errorf("Downloaded = %d, want 1", res.Downloaded)
	}
	if res.DownloadFailed != 0 {
		t.Errorf("DownloadFailed = %d, want 0 (wait canceled before Download)", res.DownloadFailed)
	}
	if res.Remaining != 4 {
		t.Errorf("Remaining = %d, want 4", res.Remaining)
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_CancelDuringSemAcquireLeavesRemaining(t *testing.T) {
	entries := makeEntries(5)
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	started0 := make(chan struct{}, len(entries))
	release := make(chan struct{})
	dl := func(context.Context, Request) (*Result, error) {
		attempts.Add(1)
		started0 <- struct{}{}
		<-release // hold the single worker slot until the test releases it
		return &Result{}, nil
	}
	go func() {
		<-started0                        // the first worker holds the only slot
		time.Sleep(20 * time.Millisecond) // let the coordinator park in the sem-acquire select
		cancel()
		time.Sleep(10 * time.Millisecond) // let it observe cancellation before the slot frees
		close(release)
	}()
	res := runPlaylist(ctx, entries, 1, 0, noWait, okBuildRequest, nil, dl)

	if got := attempts.Load(); got != 1 {
		t.Errorf("download attempts = %d, want 1 (canceled while acquiring the slot for the second)", got)
	}
	if res.Downloaded != 1 {
		t.Errorf("Downloaded = %d, want 1", res.Downloaded)
	}
	if res.Remaining != 4 {
		t.Errorf("Remaining = %d, want 4", res.Remaining)
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_PanicsRecovered(t *testing.T) {
	entries := makeEntries(4)
	buildRequest := func(_ context.Context, e PlaylistEntry) (Request, string, error) {
		if e.Index == 0 {
			panic("build request panic")
		}
		return Request{URL: e.VideoID}, "", nil
	}
	dl := func(context.Context, Request) (*Result, error) { return &Result{}, nil }
	onItem := func(o PlaylistItemOutcome) {
		if o.Entry.Index == 1 {
			panic("onItem panic")
		}
	}
	res := runPlaylist(context.Background(), entries, 2, 0, noWait, buildRequest, onItem, dl)

	if res.BuildRequestFailed != 1 {
		t.Errorf("BuildRequestFailed = %d, want 1 (the panicking build request)", res.BuildRequestFailed)
	}
	if res.Downloaded != 3 {
		t.Errorf("Downloaded = %d, want 3", res.Downloaded)
	}
	checkInvariant(t, res)
}

func TestRunPlaylist_WaitBeforeStartCalledPerAttemptInOrder(t *testing.T) {
	entries := makeEntries(4)
	dl := func(context.Context, Request) (*Result, error) { return &Result{}, nil }
	var mu sync.Mutex
	var args []int
	wait := func(_ context.Context, started int) error {
		mu.Lock()
		args = append(args, started)
		mu.Unlock()
		return nil
	}
	runPlaylist(context.Background(), entries, 3, 0, wait, okBuildRequest, nil, dl)

	if want := []int{0, 1, 2, 3}; !reflect.DeepEqual(args, want) {
		t.Errorf("waitBeforeStart started args = %v, want %v", args, want)
	}
}

func TestPickSleepWait(t *testing.T) {
	t.Run("first attempt does not wait", func(t *testing.T) {
		w := pickSleepWait(PlaylistDownloadOptions{SleepInterval: time.Hour})
		start := time.Now()
		if err := w(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("first attempt waited %v, want ~0", d)
		}
	})

	t.Run("no interval does not wait", func(t *testing.T) {
		w := pickSleepWait(PlaylistDownloadOptions{})
		start := time.Now()
		if err := w(context.Background(), 5); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("zero interval waited %v, want ~0", d)
		}
	})

	t.Run("waits at least the interval", func(t *testing.T) {
		w := pickSleepWait(PlaylistDownloadOptions{SleepInterval: 30 * time.Millisecond})
		start := time.Now()
		if err := w(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d < 25*time.Millisecond {
			t.Errorf("waited %v, want >= ~30ms", d)
		}
	})

	t.Run("canceled context returns error without a full wait", func(t *testing.T) {
		w := pickSleepWait(PlaylistDownloadOptions{SleepInterval: time.Hour})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := w(ctx, 1); err == nil {
			t.Error("want a context error, got nil")
		}
	})

	t.Run("randomized within range", func(t *testing.T) {
		w := pickSleepWait(PlaylistDownloadOptions{SleepInterval: 5 * time.Millisecond, MaxSleepInterval: 9 * time.Millisecond})
		for range 20 {
			start := time.Now()
			if err := w(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < 4*time.Millisecond {
				t.Errorf("delay %v below the floor (5ms)", d)
			}
		}
	})
}

func TestPlaylistDownloadOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		o       PlaylistDownloadOptions
		wantErr bool
	}{
		{"nil build request", PlaylistDownloadOptions{}, true},
		{"ok", PlaylistDownloadOptions{BuildRequest: okBuildRequest}, false},
		{"negative max items", PlaylistDownloadOptions{BuildRequest: okBuildRequest, MaxItems: -1}, true},
		{"negative max downloads", PlaylistDownloadOptions{BuildRequest: okBuildRequest, MaxDownloads: -1}, true},
		{"max sleep without sleep", PlaylistDownloadOptions{BuildRequest: okBuildRequest, MaxSleepInterval: time.Second}, true},
		{"max sleep below sleep", PlaylistDownloadOptions{BuildRequest: okBuildRequest, SleepInterval: 2 * time.Second, MaxSleepInterval: time.Second}, true},
		{"valid sleep range", PlaylistDownloadOptions{BuildRequest: okBuildRequest, SleepInterval: time.Second, MaxSleepInterval: 2 * time.Second}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.o.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
