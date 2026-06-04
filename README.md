# WaxTap

WaxTap is an audio-focused YouTube downloader and audio processor for Go. It is
available as both a reusable library and the `waxtap` CLI, with both surfaces
built on the same core.

WaxTap can download the best available YouTube audio or process an existing local
file. Processing stages are opt-in: transcode, cut explicit time ranges, remove
SponsorBlock segments, measure loudness, and normalize loudness. A plain download
keeps the selected source stream and does not re-encode.

> WaxTap targets public videos. Private, age-restricted, and login-gated videos
> are expected failures, not bypass targets. YouTube changes its player and
> anti-bot behavior without notice; see [MAINTENANCE.md](MAINTENANCE.md) for the
> recovery runbook and runtime knobs.

## Design at a glance

- **Library and CLI over one core.** `github.com/colespringer/waxtap` is the stable
  facade; `cmd/waxtap` is a real CLI built on the same packages.
- **Pure-Go extraction** (InnerTube + goja for the cipher). No `yt-dlp`.
- **Volatile surfaces are isolated** behind small interfaces (`youtube`,
  `youtube/internal/resolver`) so a YouTube change touches few, marked files.
- **Server-friendly:** concurrency-safe, context-cancelable, bounded memory,
  per-operation timeouts (never a single global cap), atomic temp-file output.
- **Clear about quality:** YouTube audio is lossy; FLAC/ALAC/WAV are lossless
  *re-encodes* of a lossy source. Only copy/remux avoids re-encoding.

## Package layout

| Package | Role |
|---|---|
| `waxtap` (root) | Stable facade: `Client`, `Request`/`Result`, `Options`. |
| `cmd/waxtap` | The CLI (cobra): `download`, `info`, `cut`, `normalize`, `doctor`, … |
| `format` | Audio-first `Format` model and selectors. |
| `download` | Resilient ranged/streaming download (parallel chunks, expiry refresh). |
| `transcode` | ffmpeg/ffprobe execution home (codecs, probing). |
| `cut` | Time-range cut + SponsorBlock bridge (composes `transcode`). |
| `normalize` | Loudness measure/normalize (EBU R128; track and album). |
| `sponsorblock` | SponsorBlock client + category vocabulary. |
| `potoken` | PO-token provider contract (caller-supplied). |
| `waxerr` | The domain error taxonomy (one `errors.Is` source of truth). |
| `youtube` | YouTube extraction (volatile; exported for the facade, may churn). |
| `youtube/internal/resolver` | Cipher / base.js / stream-URL resolution. |
| `internal/pipeline` | Fused probe → cut → loudness → encode pipeline. |
| `internal/httpx` | HTTP client: retry, backoff, Retry-After, per-host limiter. |
| `internal/cache` | In-memory LRU+TTL+singleflight cache. |
| `internal/diskcache` | On-disk, size-capped, schema-versioned player-JS cache. |
| `internal/tempfile` | Atomic temp-output staging + cleanup contract. |

## Requirements

- Go 1.26+
- `ffmpeg` / `ffprobe` on `PATH` (for transcode/cut/normalize; not needed for
  plain metadata or best-source downloads).

## Install

**CLI:**

```sh
go install github.com/colespringer/waxtap/cmd/waxtap@latest
```

Tagged releases also ship prebuilt binaries for Linux, macOS, and Windows on the
GitHub Releases page.

**Library:**

```sh
go get github.com/colespringer/waxtap
```

## Usage

### CLI

Media commands accept a YouTube URL or bare video/playlist ID and support
`--json` for a stable scriptable contract. `cut`, `transcode`, and `normalize`
also accept a local file, so no network is needed for local processing. Every
command has `--help`.

```sh
# Inspect audio formats (no download)
waxtap info https://youtu.be/dQw4w9WgXcQ
waxtap formats https://youtu.be/dQw4w9WgXcQ

# Download the best audio with no re-encode (the default, keep-source)
waxtap download https://youtu.be/dQw4w9WgXcQ -o track

# Download and transcode to FLAC in a single ffmpeg pass
waxtap download https://youtu.be/dQw4w9WgXcQ --transcode flac -o track.flac

# Remove SponsorBlock non-music segments and normalize loudness in one pass
waxtap download <url> --cut-sponsorblock --transcode mp3 --normalize --loudness-target -14 -o track.mp3

# Process a LOCAL file (no network)
waxtap transcode song.flac song.mp3
waxtap normalize song.wav --apply --target -14 --transcode flac -o song.flac

# Download a whole playlist into a directory, skipping already-fetched IDs
waxtap download <playlist-url> -d ./music --download-archive seen.txt

# Check extraction health
waxtap doctor
```

### Library

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/sponsorblock"
)

func main() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	// Download the best audio, remove SponsorBlock "music_offtopic" segments, and
	// transcode to FLAC in one fused ffmpeg pass.
	res, err := client.Download(context.Background(), waxtap.Request{
		URL: "https://youtu.be/dQw4w9WgXcQ",
		ProcessSpec: waxtap.ProcessSpec{
			Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
			Cut: &waxtap.CutSpec{
				SponsorBlock: []sponsorblock.Category{sponsorblock.CategoryMusicOffTopic},
				OnError:      waxtap.ProceedUncut,
			},
			Output: waxtap.ToFile("track.flac"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s -> %s (%d bytes)\n", res.VideoID, res.OutputPath, res.OutputBytes)
}
```

The default `Download` does no re-encode; all processing is opt-in. See
[`example_test.go`](example_test.go) for `Stream`, `Process` (local files),
`Enumerate` (playlists), `MeasureAlbum`, and `Info`.

## Configuration

CLI configuration is resolved in this order: explicit flag, `WAXTAP_*`
environment variable, JSON config file, built-in default. The default config file
is `config.json` under `os.UserConfigDir()/waxtap`; override it with `--config`
or `WAXTAP_CONFIG`.

Useful operational knobs:

- Cache: `waxtap cache dir`, `waxtap cache clean`, `--cache-dir`,
  `WAXTAP_CACHE_DIR`, `--no-cache`, `WAXTAP_NO_CACHE`.
- Runtime client refresh: `--profile-override`, `WAXTAP_PROFILE_OVERRIDE`, or
  `profileOverridePath` in config JSON.
- Network posture: `--proxy`, `--qps`, `--hl`, `--gl`, and their documented
  `WAXTAP_*` equivalents.
- Diagnostics: set `WAXTAP_DUMP_DIR` to write unusable YouTube responses on
  extraction failures.

`ffmpeg` and `ffprobe` are required only for processing or probing. Plain
metadata, stream resolution, and keep-source downloads do not need them.

## Maintenance

YouTube's player and anti-bot surfaces change without notice. WaxTap isolates that
volatility behind a few marked files and ships operational controls:
client-profile override files, env-gated artifact dumps, a persistent player
cache, and a `doctor` health check suitable for cron or container health probes.
See [MAINTENANCE.md](MAINTENANCE.md) for the full breakage-response runbook.

## Acknowledgements

WaxTap was heavily influenced by [kkdai/youtube](https://github.com/kkdai/youtube)
and [yt-dlp](https://github.com/yt-dlp/yt-dlp).

WaxTap is an independent implementation: it ships no code from either project and
does not invoke yt-dlp at runtime.

## Disclaimer

WaxTap is for personal and otherwise authorized use only. You are responsible for
complying with YouTube's Terms of Service and applicable law. WaxTap is
public-video only: private, age-restricted, and login-gated videos are expected
failures, not something WaxTap promises to bypass.

## License

[MIT](LICENSE) © Cole Springer.
