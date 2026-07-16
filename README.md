# WaxTap

WaxTap downloads and processes YouTube audio. It ships as a Go library and the
`waxtap` CLI, both on the same processing core. Processing is opt-in: transcode,
cut time ranges, drop SponsorBlock segments, measure or normalize loudness. A
plain download keeps the selected source stream without re-encoding.

> WaxTap targets public videos. Private, age-restricted, and login-gated videos
> are expected failures, not bypass targets. YouTube changes without notice; see
> [MAINTENANCE.md](MAINTENANCE.md) when extraction breaks.

## Highlights

- Pure-Go extraction via InnerTube and goja. No `yt-dlp` dependency.
- Token-free ANDROID_VR is the default. Full WEB audio is opt-in and needs an
  attested identity; forced iOS delivery is best-effort.
- One pure-Go pass can combine cuts, SponsorBlock removal, normalization, and
  transcoding, via the WaxFlow audio engine.
- Lossless output such as FLAC is still a re-encode of YouTube's lossy source.
  Only copy/remux avoids re-encoding.

The stable facade is the root `waxtap` package. YouTube code is isolated under
`youtube`; audio processing lives in `internal/media` (a WaxFlow-backed engine)
and `internal/pipeline`.

## Requirements

- Go 1.26+

WaxTap is a single static binary with no external runtime dependency: all audio
work (transcode, cut, normalize, probe) runs in-process via the pure-Go WaxFlow
engine.

## Install

```sh
go install github.com/colespringer/waxtap/v3/cmd/waxtap@latest   # CLI
go get github.com/colespringer/waxtap/v3                         # library
```

[Release archives](https://github.com/colespringer/waxtap/releases/latest) hold
Linux, macOS, and Windows binaries for amd64 and arm64. Put `waxtap` on `PATH`
and run `waxtap --help`. Unsigned macOS binaries may need
`xattr -d com.apple.quarantine /path/to/waxtap`; Windows may prompt SmartScreen.

## CLI

Media commands accept a YouTube URL or bare video or playlist ID. `download`
also accepts a channel URL or bare `UC` ID, resolving to the channel's uploads
feed. `cut`, `transcode`, and `normalize` also take local files. Every command
has `--help`, and `--json` is a stable scriptable contract (`schemaVersion` 2).

```sh
waxtap info <video-url>                         # metadata and best audio
waxtap formats <video-url>                      # all audio formats
waxtap download <video-url> -o track            # keep source
waxtap download <video-url> --format flac -o track.flac
waxtap download <video-url> --sponsorblock --normalize --format mp3 -o track.mp3

waxtap transcode song.flac song.mp3
waxtap normalize song.wav --loudness-target -14 --format flac -o song.flac
waxtap normalize --album --format flac --dir ./normalized ./album/*.flac

waxtap download <playlist-url> -d ./music --download-archive archive.txt
waxtap download <channel-url> -d ./music        # channel uploads, newest first
waxtap download <channel-url> --list            # list entries, no download
waxtap doctor
```

`info --show-url` adds a signed, expiring stream URL and content length under
`resolved.*`; treat that output as sensitive. `info --full` adds publish date
and chapters via a token-free watch-page fetch.

`transcode` and `normalize` also take directories: `-r` recurses, `--dir` sets
an output directory, and `--force` re-encodes files already in the target
codec. Album normalization preserves relative track loudness. Loudness uses EBU
R128 (integrated LUFS, true peak dBTP, range LU).

### Notes

- `--channels mono|stereo|surround|any` picks a native layout, defaulting to
  stereo. `--downmix` allows surround-to-stereo/mono; it never upmixes.
- `--no-fallback` disables watch-page, WEB-context, and incomplete-download
  fallbacks. Results report the client that actually delivered.
- Playlist downloads support `--concurrency`, pacing, attempt limits, collision
  policies, and yt-dlp-compatible `--download-archive` files.
- `waxtap cache dir` and `waxtap cache clean` manage the persistent player-JS
  cache; `--no-cache` disables it.

### Exit codes

The CLI maps failures to stable exit codes. The same class appears in `--json`
as `error.code`. Run `waxtap exit-codes` for the built-in table.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unclassified error |
| 2 | invalid request/config, including a playlist or channel URL passed to a video command, unsupported input, or unavailable requested format |
| 3 | unavailable/restricted video or playlist (private, age-restricted, members-only, geo-blocked, removed), login required, live or upcoming, or no audio |
| 4 | extraction, cipher, or playlist parsing failure; WaxTap may need an update |
| 5 | rate limited |
| 6 | retired (formerly ffmpeg/ffprobe not found) |
| 7 | incomplete stream or expired stream URL |
| 8 | PO token required, missing, rejected, or not minted |
| 9 | network failure, including an unreachable proxy or sidecar |
| 10 | local I/O failure |
| 130 | canceled with SIGINT |

Malformed targets exit 2; a well-formed but nonexistent or private video can
only be classified after a network request and exits 3.

## Library

```go
package main

import (
	"context"
	"log"

	"github.com/colespringer/waxtap/v3"
)

func main() {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	_, err = client.Download(context.Background(), waxtap.Request{
		URL: "https://youtu.be/VIDEO_ID_01",
		ProcessSpec: waxtap.ProcessSpec{
			Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
			Output:    waxtap.ToFile("track.flac"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

A default `Download` (nil `ProcessSpec`) delivers the source stream
byte-for-byte: no processing, `SourceBytes == OutputBytes`, `Transcoded` false.
Library selection starts from `LayoutAny` and can rank a surround track highest;
pass `WithChannels(LayoutStereo)` to match the CLI. `Client.Enumerate` expands a
playlist or channel URL with `Skip`/`Stop` predicates for an archive cursor, and
`WithFullMetadata()` adds publish date and chapters.

Availability failures (`ErrVideoUnavailable`, `ErrAgeRestricted`,
`ErrMembersOnly`, `ErrGeoBlocked`, `ErrLiveContent`, `ErrLiveNotStarted`, and
siblings) are typed sentinels a feed consumer should skip rather than treat as
fatal; see the package doc's skip-vs-fail taxonomy. For full WEB SABR audio,
wire a sidecar through `NewSidecarPOTokenProvider`,
`NewSidecarPlayerContextProvider`, or `NewSidecarSessionProvider` (see below).
[`example_test.go`](example_test.go) covers streaming, local processing,
playlists, SponsorBlock, album measurement, metadata, and WEB SABR.

## Configuration

CLI precedence, highest to lowest: explicit flag, `WAXTAP_*` environment
variable, JSON config file, built-in default. The default file is `config.json`
under `os.UserConfigDir()/waxtap`; override with `--config` or `WAXTAP_CONFIG`.
Unknown JSON keys and malformed environment values are errors.

`--json`, `--quiet`, and `--verbose` are global. Other flags appear only on the
commands that use them. Timeout values are seconds; keys with no flag are
config/environment only.

| Config key | Environment variable | Flag |
|---|---|---|
| `cacheDir` | `WAXTAP_CACHE_DIR` | `--cache-dir` |
| `noCache` | `WAXTAP_NO_CACHE` | `--no-cache` |
| `tempDir` | `WAXTAP_TEMP_DIR` | `--temp-dir` |
| `proxy` | `WAXTAP_PROXY` | `--proxy` |
| `insecure` | `WAXTAP_INSECURE` | `--insecure` |
| `perHostQPS` | `WAXTAP_QPS` | `--qps` |
| `cooldownSeconds` | `WAXTAP_COOLDOWN` | `--cooldown` |
| `hl` | `WAXTAP_HL` | `--hl` |
| `gl` | `WAXTAP_GL` | `--gl` |
| `sponsorBlockBaseURL` | `WAXTAP_SPONSORBLOCK_BASE_URL` | `--sponsorblock-url` |
| `profileOverridePath` | `WAXTAP_PROFILE_OVERRIDE` | `--profile-override` |
| `chromeMajor` | `WAXTAP_CHROME_MAJOR` | `--chrome-major` |
| `poTokenURL` | `WAXTAP_POTOKEN_URL` | `--potoken-url` |
| `playerContextURL` | `WAXTAP_PLAYER_CONTEXT_URL` | `--player-context-url` |
| `client` | `WAXTAP_CLIENT` | `--client` |
| `sessionURL` | `WAXTAP_SESSION_URL` | `--session-url` |
| `visitorData` | `WAXTAP_VISITOR_DATA` | `--visitor-data` |
| `cookies` | `WAXTAP_COOKIES` | `--cookies` |
| `apiKey` | `WAXTAP_API_KEY` | `--api-key` |
| `channels` | `WAXTAP_CHANNELS` | `--channels` |
| `downmix` | `WAXTAP_DOWNMIX` | `--downmix` |
| `downloadConcurrency` | `WAXTAP_DOWNLOAD_CONCURRENCY` | `--concurrency` (download) |
| `procs` | `WAXTAP_PROCS` | - |
| `chunkParallelism` | `WAXTAP_CHUNKS` | - |
| `extractionTimeoutSeconds` | `WAXTAP_EXTRACTION_TIMEOUT` | - |
| `resolveTimeoutSeconds` | `WAXTAP_RESOLVE_TIMEOUT` | - |
| `webContextTimeoutSeconds` | `WAXTAP_WEB_CONTEXT_TIMEOUT` | - |
| `sponsorBlockTimeoutSeconds` | `WAXTAP_SPONSORBLOCK_TIMEOUT` | - |
| `chunkTimeoutSeconds` | `WAXTAP_CHUNK_TIMEOUT` | - |

## PO tokens and WEB

ANDROID_VR is token-free for public videos. WEB-family clients use URL-less
SABR/UMP audio, and complete delivery needs three things together: a GVS-scope
PO-token provider (`Options.POTokenProvider` or `--potoken-url`), an attested
identity (a `/player-context` handoff or an adopted `/session`), and a shared
egress IP for the attesting service and the download. A PO token alone does not
lift the WEB preview cap.

```sh
# Attested player context, adopted WEB session as fallback
waxtap download <url> --client web \
  --player-context-url http://127.0.0.1:4416/player-context \
  --session-url http://127.0.0.1:4416/session \
  --potoken-url http://127.0.0.1:4416/get_pot
```

WaxTap tries the attested player context first; if it fails or caps, the WEB
chain can use the adopted session. Static adoption is also available with
`--visitor-data` and optional `--cookies`. Library callers get the same handoff
via the `NewSidecar*` providers, each taking a base URL or full endpoint plus an
optional `WithSidecarAPIKey`; `ParseNetscapeCookies` loads a static session from
a browser `cookies.txt`. See [MAINTENANCE.md](MAINTENANCE.md) for sidecar
contracts and SABR diagnostics.

## Maintenance

`waxtap doctor` runs a low-cost extraction, resolution, and byte-read health
check; `waxtap doctor --full` verifies complete delivery. The
[maintenance runbook](MAINTENANCE.md) covers dumps, profile refreshes, cipher
failures, SABR changes, fixtures, and releases.

## Acknowledgements

WaxTap was influenced by [kkdai/youtube](https://github.com/kkdai/youtube) and
[yt-dlp](https://github.com/yt-dlp/yt-dlp), but ships no code from either and
does not invoke yt-dlp.

## Disclaimer

WaxTap is for personal and otherwise authorized use. You are responsible for
complying with YouTube's Terms of Service and applicable law.

## License

[MIT](LICENSE).
