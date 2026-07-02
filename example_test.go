package waxtap_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/colespringer/waxtap/v2"
)

// ExampleClient_Download downloads the best audio stream to a file. With no
// processing requested, WaxTap keeps the source encoding.
func ExampleClient_Download() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.Download(context.Background(), waxtap.Request{
		URL: "https://www.youtube.com/watch?v=VIDEO_ID_01",
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile("track.opus"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s -> %s (%d bytes)\n", res.VideoID, res.OutputPath, res.OutputBytes)
}

// ExampleClient_Download_transcodeAndSponsorBlock downloads a video, removes
// SponsorBlock "music_offtopic" segments, and transcodes to FLAC in one ffmpeg
// pass. SourcePolicy defaults to MinimizeLoss.
func ExampleClient_Download_transcodeAndSponsorBlock() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.Download(context.Background(), waxtap.Request{
		URL: "https://www.youtube.com/watch?v=VIDEO_ID_01",
		ProcessSpec: waxtap.ProcessSpec{
			Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
			Cut: &waxtap.CutSpec{
				SponsorBlock: []waxtap.Category{waxtap.CategoryMusicOffTopic},
				OnError:      waxtap.ProceedUncut,
			},
			Output: waxtap.ToFile("track.flac"),
			Events: func(e waxtap.Event) {
				if e.Stage == waxtap.StageWarning && e.Warning != nil {
					log.Printf("warning: %s", e.Warning.Detail)
				}
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("cut=%v transcoded=%v\n", res.CutApplied, res.Transcoded)
}

// ExampleClient_Stream pipes the audio to an arbitrary writer (here a file)
// without staging to a temp file when no processing is requested.
func ExampleClient_Stream() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	rc, info, err := client.Stream(context.Background(), waxtap.Request{
		URL: "https://youtu.be/VIDEO_ID_01",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer rc.Close()

	out, err := os.Create("track" + "." + info.Format.Extension)
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, rc); err != nil {
		log.Fatal(err)
	}
}

// ExampleClient_Process transcodes a local file and normalizes its loudness to
// -14 LUFS, fused into the same encode. No YouTube access occurs.
func ExampleClient_Process() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.Process(context.Background(), waxtap.ProcessRequest{
		Input: "song.wav",
		ProcessSpec: waxtap.ProcessSpec{
			Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatMP3},
			Loudness:  &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: -14},
			Output:    waxtap.ToFile("song.mp3"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if res.Loudness != nil && res.Loudness.Output != nil {
		fmt.Printf("normalized to %.1f LUFS\n", res.Loudness.Output.IntegratedLUFS)
	}
}

// ExampleClient_Enumerate lists a playlist without downloading any audio.
func ExampleClient_Enumerate() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	pl, err := client.Enumerate(context.Background(),
		"https://www.youtube.com/playlist?list=UUSMOQeBJ2RAnuFungnQOxLg",
		waxtap.EnumerateOptions{MaxItems: 50},
	)
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range pl.Entries {
		// The video ID is the stable key WaxTap uses for deduplication.
		fmt.Printf("%d. %s (%s)\n", entry.Index, entry.Title, entry.VideoID)
	}
}

// ExampleClient_DownloadPlaylist downloads up to ten playlist entries one at a
// time, waiting between downloads. BuildRequest prepares or skips each entry;
// OnItem receives the outcome for every entry the run reaches.
func ExampleClient_DownloadPlaylist() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.DownloadPlaylist(context.Background(),
		"https://www.youtube.com/playlist?list=UUSMOQeBJ2RAnuFungnQOxLg",
		waxtap.PlaylistDownloadOptions{
			Concurrency:   1,               // serialize downloads
			SleepInterval: 5 * time.Second, // pause between them
			MaxDownloads:  10,              // stop after 10 attempts
			BuildRequest: func(_ context.Context, e waxtap.PlaylistEntry) (waxtap.Request, string, error) {
				return waxtap.Request{
					URL:         e.VideoID,
					ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(e.VideoID + ".opus")},
				}, "", nil
			},
			OnItem: func(o waxtap.PlaylistItemOutcome) {
				switch {
				case o.Err != nil:
					log.Printf("%s: %v", o.Entry.VideoID, o.Err)
				case o.SkipReason != "":
					log.Printf("%s: skipped (%s)", o.Entry.VideoID, o.SkipReason)
				}
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d downloaded, %d remaining (cap reached: %v)\n",
		res.Downloaded, res.Remaining, res.CapReached)
}

// ExampleClient_Measure reports the loudness of a single local file without
// writing any output.
func ExampleClient_Measure() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	loud, err := client.Measure(context.Background(), "song.flac")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%.1f LUFS\n", loud.IntegratedLUFS)
}

// ExampleClient_MeasureAlbum measures several files as one album, useful for
// ReplayGain-style album tags without rewriting the files.
func ExampleClient_MeasureAlbum() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	album, err := client.MeasureAlbum(context.Background(), []string{
		"01.flac", "02.flac", "03.flac",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("album: %.2f LUFS\n", album.Album.IntegratedLUFS)
	for i, tr := range album.PerTrack {
		fmt.Printf("track %d: %.2f LUFS\n", i+1, tr.IntegratedLUFS)
	}
}

// ExampleClient_Info fetches metadata and candidate audio formats without
// downloading.
func ExampleClient_Info() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	video, err := client.Info(context.Background(), "VIDEO_ID_01", waxtap.InfoBasic)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s by %s (%d formats)\n", video.Title, video.Author, len(video.Formats))
}

// Example_errors shows the two ways to inspect a failure from Download or Info:
// errors.Is for the sentinel category, and errors.As for YouTube's structured
// detail. The re-exported error types satisfy error on their pointer type, so
// errors.As takes a double-pointer target (var pe *PlayabilityError; &pe).
func Example_errors() {
	// In real use this comes from client.Download/Info; constructed here for a
	// self-contained example.
	var err error = &waxtap.PlayabilityError{
		Status:   "UNPLAYABLE",
		Reason:   "This video is unavailable",
		Sentinel: waxtap.ErrVideoUnavailable,
	}

	if errors.Is(err, waxtap.ErrVideoUnavailable) {
		fmt.Println("category: video unavailable")
	}

	var pe *waxtap.PlayabilityError
	if errors.As(err, &pe) {
		fmt.Printf("status: %s\n", pe.Status)
	}
	// Output:
	// category: video unavailable
	// status: UNPLAYABLE
}

// ExampleNewSidecarPOTokenProvider wires a running WaxSeal sidecar into a client
// for full WEB SABR audio: a PO-token provider plus an attested player-context
// provider, both pointed at the same host so the token mint and the stream share
// egress. NewSidecarSessionProvider (adopted /session) is the alternative to the
// player-context handoff. See MAINTENANCE.md for the sidecar wire contracts.
func ExampleNewSidecarPOTokenProvider() {
	// The PO-token and player-context endpoints must share a host with the
	// download so the IP-bound token stays valid. Each accepts a base URL or a
	// full endpoint; add WithSidecarAPIKey for an authenticated sidecar.
	poToken, err := waxtap.NewSidecarPOTokenProvider("http://127.0.0.1:4416")
	if err != nil {
		log.Fatal(err)
	}
	playerContext, err := waxtap.NewSidecarPlayerContextProvider("http://127.0.0.1:4416")
	if err != nil {
		log.Fatal(err)
	}

	client, err := waxtap.New(waxtap.Options{
		POTokenProvider:       poToken,
		PlayerContextProvider: playerContext,
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.Download(context.Background(), waxtap.Request{
		URL:         "https://www.youtube.com/watch?v=VIDEO_ID_01",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile("track.opus")},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("delivered %d bytes via %s\n", res.OutputBytes, res.Client)
}
