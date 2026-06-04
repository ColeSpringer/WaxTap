package waxtap_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/sponsorblock"
)

// ExampleClient_Download downloads the best audio stream to a file without
// re-encoding, the default keep-source behavior.
func ExampleClient_Download() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.Download(context.Background(), waxtap.Request{
		URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile("track.opus"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s -> %s (%d bytes)\n", res.VideoID, res.OutputPath, res.OutputBytes)
}

// ExampleClient_Download_transcodeAndSponsorBlock downloads, removes SponsorBlock
// "music_offtopic" segments, and transcodes to FLAC in a single fused ffmpeg
// pass. SourcePolicy defaults to MinimizeLoss.
func ExampleClient_Download_transcodeAndSponsorBlock() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.Download(context.Background(), waxtap.Request{
		URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		ProcessSpec: waxtap.ProcessSpec{
			Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
			Cut: &waxtap.CutSpec{
				SponsorBlock: []sponsorblock.Category{sponsorblock.CategoryMusicOffTopic},
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
		URL: "https://youtu.be/dQw4w9WgXcQ",
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
		"https://www.youtube.com/playlist?list=PLFgquLnL59alCl_2TQvOiD5Vgm1hCaGSI",
		waxtap.EnumerateOptions{MaxItems: 50},
	)
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range pl.Entries {
		// The video ID is the stable key WaxBin dedupes on.
		fmt.Printf("%d. %s (%s)\n", entry.Index, entry.Title, entry.VideoID)
	}
}

// ExampleClient_MeasureAlbum measures a set of files as an album for ReplayGain
// album-gain tagging, the non-destructive path for library-wide consistency.
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

	video, err := client.Info(context.Background(), "dQw4w9WgXcQ", waxtap.InfoBasic)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s by %s (%d formats)\n", video.Title, video.Author, len(video.Formats))
}
