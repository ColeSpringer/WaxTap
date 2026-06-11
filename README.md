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
- **Pure-Go extraction** (InnerTube + goja for the cipher). No `yt-dlp`. The
  default ANDROID_VR and iOS clients return playable audio for public videos with
  no PO token. Full WEB audio over SABR/UMP is opt-in: it needs a GVS
  `potoken.Provider` plus an attested `/player-context` handoff (WaxTap's own WEB
  `/player` only earns a ~1-minute preview); see [PO tokens & WEB](#po-tokens--web).
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
| `youtube/internal/sabr` | SABR/UMP streaming for URL-less WEB-family audio. |
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

Install either the CLI or the Go package. The CLI is meant to run from a shell;
the release archives do not install a desktop app. See [Using the prebuilt
binaries](#using-the-prebuilt-binaries) for platform notes.

**With Go:**

```sh
go install github.com/colespringer/waxtap/cmd/waxtap@latest
```

This installs `waxtap` into `$(go env GOBIN)` (or `$(go env GOPATH)/bin`); make
sure that directory is on your `PATH`.

**Prebuilt binaries:** tagged releases include Linux, macOS, and Windows
archives (amd64/arm64) on the GitHub Releases page.

**Library:**

```sh
go get github.com/colespringer/waxtap
```

### Using the prebuilt binaries

Each release archive contains the `waxtap` executable and documentation. Extract
the archive for your platform, put the executable somewhere on your `PATH`, then
run it from a terminal like any other CLI.

**Linux / macOS:**

```sh
# 1. Extract the archive for your platform
tar -xzf waxtap_*_linux_amd64.tar.gz      # or _darwin_arm64, etc.

# 2. Move it to a directory on your PATH
sudo mv waxtap /usr/local/bin/            # or ~/.local/bin, ~/bin, etc.

# 3. Run it from a terminal
waxtap --help
```

The archive preserves the executable bit; a standalone downloaded binary may need
`chmod +x waxtap` first.

> **macOS Gatekeeper:** the binaries are not code-signed, so macOS may block the
> first launch ("cannot be opened because Apple cannot check it for malware" /
> "developer cannot be verified"). Clear the quarantine flag for the installed
> binary:
>
> ```sh
> xattr -d com.apple.quarantine /usr/local/bin/waxtap
> ```
>
> You can also right-click the binary in Finder and choose **Open** the first
> time.

**Windows:**

1. Unzip `waxtap_*_windows_amd64.zip`.
2. Move `waxtap.exe` into a folder on your `PATH` (or add its folder to `PATH`).
3. Open **PowerShell** or **Command Prompt** and run `waxtap --help`.

> **SmartScreen:** because the `.exe` is not signed, Windows may show "Windows
> protected your PC". Choose **More info > Run anyway** on first launch.

## Usage

### CLI

Media commands accept a YouTube URL or bare video/playlist ID and support
`--json` for a stable scriptable contract. `cut`, `transcode`, and `normalize`
also accept a local file, so no network is needed for local processing. Every
command has `--help`.

```sh
# Inspect audio formats (no download)
waxtap info <video-url>
waxtap formats <video-url>

# Download the best audio with no re-encode (the default, keep-source)
waxtap download <video-url> -o track

# Download and transcode to FLAC in a single ffmpeg pass
waxtap download <video-url> --transcode flac -o track.flac

# Remove SponsorBlock non-music segments and normalize loudness in one pass
waxtap download <video-url> --cut-sponsorblock --transcode mp3 --normalize --loudness-target -14 -o track.mp3

# Process a LOCAL file (no network)
waxtap transcode song.flac song.mp3
waxtap normalize song.wav --apply --target -14 --transcode flac -o song.flac

# Download a whole playlist into a directory, skipping already-fetched IDs
waxtap download <playlist-url> -d ./music --download-archive seen.txt

# Download serially, waiting 5 seconds between downloads, up to 10 attempts
waxtap download <playlist-url> -d ./music --concurrency 1 --sleep-interval 5s --max-downloads 10

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
	// transcode to FLAC in one ffmpeg pass.
	res, err := client.Download(context.Background(), waxtap.Request{
		URL: "https://youtu.be/VIDEO_ID_01",
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
`Enumerate` and `DownloadPlaylist` (playlists), `MeasureAlbum`, and `Info`.
`DownloadPlaylist` downloads playlist entries with bounded concurrency, optional
pacing, and an optional limit on download attempts.

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
- Built-in Chrome identity: use `--chrome-major`, `WAXTAP_CHROME_MAJOR`, or
  `chromeMajor` in config JSON to override the emulated Chrome major without a
  rebuild. This cannot be combined with `--profile-override`, which supplies its
  own user agents.
- Single client / session adoption: `--client web|ios|android_vr|web_embedded`
  forces one built-in client as the whole chain (conflicts with `--profile-override`).
  `--visitor-data` (+ optional `--cookies`) or `--session-url` adopt an external
  guest session for byte-exact coherence with a token minter; see
  [PO tokens & WEB](#po-tokens--web).
- Network posture: `--proxy`, `--qps`, `--cooldown`, `--hl`, `--gl`, and their
  documented `WAXTAP_*` equivalents. `--cooldown` (or `WAXTAP_COOLDOWN`, seconds)
  pauses requests to a host after HTTP 429, or after HTTP 503/403 with a
  `Retry-After` header. A longer `Retry-After` value takes precedence, up to the
  retry-wait limit.
- Playlist pacing (download command): `--sleep-interval` sets the minimum delay
  before each download after the first. `--max-sleep-interval` adds a randomized
  upper bound, and `--max-downloads` limits download attempts; skipped entries
  and resolution failures do not count. With `--concurrency 1`, the interval
  falls between completed downloads.
- Diagnostics: set `WAXTAP_DUMP_DIR` to write unusable YouTube responses on
  extraction failures, and `WAXTAP_SABR_DUMP_DIR` to write each raw SABR
  round for offline inspection.

`ffmpeg` and `ffprobe` are required only for processing or probing. Plain
metadata, stream resolution, and keep-source downloads do not need them.

## PO tokens & WEB

ANDROID_VR and iOS return playable audio for public videos with **no PO token**,
and they are WaxTap's zero-dependency default. Everything below is opt-in, for
callers who specifically want the WEB path.

The WEB-family clients serve URL-less audio over SABR/UMP and need a GVS-scope
`potoken.Provider`; WaxTap ships no token generator (supply one via
`Options.POTokenProvider`, or the CLI's `--potoken-url` for a bgutil server).

### Full WEB audio: the attested `/player-context` handoff

A WEB `/player` call WaxTap makes itself only earns a **~1-minute preview**
(YouTube's anti-automation grade: `STREAM_PROTECTION_STATUS=2`). Full delivery
(`status 1`) is baked into a `serverAbrStreamingUrl` minted by an **attested
browser** that has actually begun playback. So for complete WEB audio, WaxTap
consumes a streaming **context** from an external attesting browser (e.g. a
WaxSeal `/player-context` endpoint) instead of building its own preview-grade URL:

```sh
waxtap download <url> \
  --player-context-url http://127.0.0.1:4416 \
  --potoken-url        http://127.0.0.1:4416
```

The provider returns snake_case JSON: `{ player_url, server_abr_streaming_url
(scrambled n), video_playback_ustreamer_config, visitor_data, client_version,
audio_formats, title, length_seconds, … }`; each `audio_formats` entry carries
`itag, lmt, xtags, mime_type, …` plus optional `is_drc` / `audio_track_id` for
DRC and multi-audio renditions. WaxTap descrambles `n` with the context's
`player_url`, mints a GVS token bound to its `visitor_data`, picks a format, and
streams the file through its existing SABR loop. Go-side, cold start, no
browser. Wire it as `Options.PlayerContextProvider`.

`--player-context-url` requires `--potoken-url` (the stream binds a GVS token to
the context's `visitor_data`), and the context mint and the download **must share
an egress IP** (the signed URL is IP-bound). When the WEB context is unavailable,
WaxTap logs a `web-context-fallback` warning and falls back to the default chain
(or the forced `--client` chain) so the download still succeeds; after a provider
failure the attempt is skipped for a short cooldown so a dead sidecar does not
tax every video in a batch. Each context fetch is bounded by
`webContextTimeoutSeconds` / `WAXTAP_WEB_CONTEXT_TIMEOUT` (default 20s).

### Session adoption (byte-exact identity)

For byte-exact session coherence with a minter, WaxTap can also **adopt an
external guest session** instead of bootstrapping its own, so it streams under the
exact identity a real browser attested. Adoption requires a uniform client chain:

```sh
# Force WEB and adopt a session from a /session endpoint (e.g. a token minter):
waxtap download <url> --client web \
  --session-url http://127.0.0.1:4417/session \
  --potoken-url http://127.0.0.1:4417
# Or a static session: the browser's exact X-Goog-Visitor-Id literal (+ cookies):
waxtap download <url> --client web --visitor-data 'Cgt...%3D%3D' --cookies ./cookies.txt
```

Notes: the adopted `visitorData` must be the browser's exact `X-Goog-Visitor-Id`
literal (sent verbatim); the session must be a logged-out **guest** (login cookies
are dropped); the minter host and downloads must share an egress IP (use `--proxy`
to align them). The session resolves once per client, so long-running services
should construct a fresh client per task.

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
