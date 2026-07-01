# WaxTap

WaxTap downloads and processes YouTube audio. It is available as a Go library
and as the `waxtap` command-line tool. Both use the same processing core.

Processing is opt-in: transcode, cut time ranges, remove SponsorBlock segments,
measure loudness, or normalize loudness. A plain download keeps the selected
source stream without re-encoding.

> WaxTap targets public videos. Private, age-restricted, and login-gated videos
> are expected failures, not bypass targets. YouTube changes without notice; see
> [MAINTENANCE.md](MAINTENANCE.md) when extraction breaks.

## Highlights

- Pure-Go YouTube extraction using InnerTube and goja; no `yt-dlp` dependency.
- Token-free ANDROID_VR is the default client. Forced iOS delivery is
  best-effort. Full WEB audio is opt-in and requires an attested identity.
- Context cancellation, bounded memory, per-operation timeouts, resilient
  ranged downloads, and atomic output.
- One ffmpeg pass can combine cuts, SponsorBlock removal, normalization, and
  transcoding.
- Lossless formats such as FLAC are still re-encodes of YouTube's lossy source.
  Only copy/remux avoids re-encoding.

The stable library facade is the root `waxtap` package. YouTube-specific code is
isolated under `youtube`; processing lives under `cut`, `normalize`,
`transcode`, and `internal/pipeline`.

## Requirements

- Go 1.26+
- `ffmpeg` and `ffprobe` for transcoding, cutting, normalization, and probing.
  Plain metadata and keep-source downloads do not need them.

## Install

```sh
# CLI
go install github.com/colespringer/waxtap/cmd/waxtap@latest

# Library
go get github.com/colespringer/waxtap
```

[Release archives](https://github.com/colespringer/waxtap/releases/latest)
contain Linux, macOS, and Windows binaries for amd64 and arm64. Extract the
archive, put `waxtap` or `waxtap.exe` on `PATH`, and run `waxtap --help`.
Unsigned macOS binaries may need
`xattr -d com.apple.quarantine /path/to/waxtap`; Windows may require approving
the first SmartScreen prompt.

## CLI

Media commands accept a YouTube URL or bare video or playlist ID; `download`
also accepts a channel URL (a `/channel/`, `/@handle`, `/c/`, or `/user/` link,
or a bare `UC` ID), which resolves to the channel's uploads feed. `cut`,
`transcode`, and `normalize` also accept local files. Every command has `--help`,
and `--json` provides a stable scriptable contract (`schemaVersion` 1). `info
--show-url` adds a signed, expiring stream URL at `resolved.url`, plus
`resolved.expiresAt` and `resolved.contentLength`; treat captured output as
sensitive. `info --full` adds the publish date and chapters via a token-free
watch-page fetch.

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
waxtap download <channel-url> -d ./music         # the channel's uploads feed
waxtap download <channel-url> --list             # list entries, no download
waxtap doctor
```

Channel and playlist enumeration returns entries in feed order; a channel's
uploads feed is newest-first and lists Shorts and past live streams as ordinary
entries.

Directory inputs are supported by `transcode` and `normalize`; use `-r` to
recurse, `--dir` for an output directory, and `--force` to re-encode files that
already match the target codec. Album normalization preserves relative track
loudness. Loudness values use EBU R128: integrated loudness in LUFS, true peak
in dBTP, and range in LU.

### Exit codes

The CLI maps failures to stable process exit codes. The same class appears in
`--json` as `error.code`.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unclassified error |
| 2 | invalid request/config, including a playlist or channel URL passed to a video command, unsupported input, or unavailable requested format |
| 3 | unavailable/restricted video (private, age-restricted, members-only, geo-blocked, removed) or playlist, login required, live or upcoming content, or no audio |
| 4 | extraction, cipher, or playlist parsing failure; WaxTap may need an update |
| 5 | rate limited |
| 6 | ffmpeg/ffprobe not found |
| 7 | incomplete stream or expired stream URL |
| 8 | PO token required, missing, rejected, or not minted |
| 9 | network failure, including an unreachable proxy or sidecar |
| 10 | local I/O failure |
| 130 | canceled with SIGINT |

Run `waxtap exit-codes` for the built-in table. Malformed targets exit 2; a
well-formed but nonexistent or private video can only be classified after a
network request and exits 3.

Sidecar failures are classified by cause: a configuration-related 4xx exits 2,
HTTP 429 exits 5, and an unreachable endpoint, server failure, or invalid
response exits 9.

## Library

```go
package main

import (
	"context"
	"log"

	"github.com/colespringer/waxtap"
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

The default `Download` (a nil `ProcessSpec`) delivers the source stream
byte-for-byte: no ffmpeg, `SourceBytes == OutputBytes`, and `Transcoded` false.
Library selection starts from `LayoutAny`, so it can rank a surround track
highest; pass `WithChannels(LayoutStereo)` to match the CLI (see Operational
notes). `Client.Enumerate` expands a playlist or channel URL, with `Skip`/`Stop`
predicates for an archive cursor; `Info(..., WithFullMetadata())` and
`Request.FullMetadata` add publish date and chapters. Availability failures
(`ErrVideoUnavailable`, `ErrAgeRestricted`, `ErrMembersOnly`, `ErrGeoBlocked`,
`ErrLiveContent`, `ErrLiveNotStarted`, and siblings) are typed sentinels a feed
consumer should skip rather than treat as fatal; see the package doc's
skip-vs-fail taxonomy. For full WEB SABR audio, wire a running sidecar through
`NewSidecarPOTokenProvider`/`NewSidecarPlayerContextProvider`/`NewSidecarSessionProvider`
(see PO tokens and WEB). See [`example_test.go`](example_test.go) for streaming,
local processing, playlists, SponsorBlock, album measurement, metadata, and WEB
SABR examples.

## Configuration

From highest to lowest, CLI configuration precedence is: explicit flag,
`WAXTAP_*` environment variable, JSON config file, then built-in default. The
default file is `config.json` under `os.UserConfigDir()/waxtap`; override it
with `--config` or `WAXTAP_CONFIG`. Unknown JSON keys and malformed environment
values are errors.

`--json`, `--quiet`, and `--verbose` are global. Other flags appear only on
commands that use them.

### Config keys

Keys with no flag are config/environment only. Timeout values are seconds.

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
| `ffmpegProcs` | `WAXTAP_FFMPEG_PROCS` | - |
| `chunkParallelism` | `WAXTAP_CHUNKS` | - |
| `extractionTimeoutSeconds` | `WAXTAP_EXTRACTION_TIMEOUT` | - |
| `resolveTimeoutSeconds` | `WAXTAP_RESOLVE_TIMEOUT` | - |
| `webContextTimeoutSeconds` | `WAXTAP_WEB_CONTEXT_TIMEOUT` | - |
| `sponsorBlockTimeoutSeconds` | `WAXTAP_SPONSORBLOCK_TIMEOUT` | - |
| `chunkTimeoutSeconds` | `WAXTAP_CHUNK_TIMEOUT` | - |

### Operational notes

- `--client web|ios|android_vr|web_embedded` forces one built-in client.
  `--profile-override` replaces the full client chain, and `--chrome-major`
  refreshes only the built-in WEB-family identity. Avoid forcing `--client ios`
  for audio downloads: metadata usually resolves, but media delivery is
  unreliable and can fail even on short clips. Use android_vr or an attested WEB
  profile for audio.
- `--channels mono|stereo|surround|any` selects a native layout when possible and
  defaults to stereo. `--downmix` permits surround-to-mono/stereo conversion; it
  never upmixes. Library callers start with `LayoutAny`, which can rank a
  surround track highest; pass `WithChannels` to match the CLI's constraint.
- Available metadata varies by client: WEB exposes the published date, while iOS
  exposes DRC (loudness-normalized) format variants. This is expected.
- `--no-fallback` disables watch-page, WEB-context, and incomplete-download
  fallbacks. Results report the client that actually delivered them.
- Playlist downloads support `--concurrency`, pacing, attempt limits, collision
  policies, and yt-dlp-compatible `--download-archive` files. WaxTap writes
  `youtube <id>` entries and also reads bare-ID entries.
- `waxtap cache dir` and `waxtap cache clean` manage the persistent player-JS
  cache. Set `WAXTAP_DUMP_DIR` or `WAXTAP_SABR_DUMP_DIR` for diagnostic dumps.
- Sidecar URL flags accept a base URL or full endpoint. `--api-key` sends
  `X-API-Key`; sidecars bypass `--proxy`, do not follow redirects, and should
  use HTTPS when remote.

## PO tokens and WEB

ANDROID_VR is the token-free default for public videos. WEB-family clients use
URL-less SABR/UMP audio. Complete WEB delivery requires:

1. A GVS-scope PO-token provider (`Options.POTokenProvider` or
   `--potoken-url`).
2. An attested identity from either a `/player-context` handoff or adopted
   `/session`.
3. A shared egress IP for the attesting service and the download.

A PO token by itself is not a portable way to lift the WEB preview cap. Full WEB
audio can work when WaxTap and WaxSeal share the same attested egress.

```sh
# Attested player context
waxtap download <url> \
  --player-context-url http://127.0.0.1:4416 \
  --potoken-url http://127.0.0.1:4416

# Adopt a guest browser session
waxtap download <url> --client web \
  --session-url http://127.0.0.1:4417/session \
  --potoken-url http://127.0.0.1:4417
```

If one sidecar exposes all three routes, you can configure both WEB handoff paths
in one command. WaxTap tries the attested player context first. If that path
fails or caps, the WEB client chain can still use the adopted session from
`--session-url`.

```sh
# Player context first, adopted WEB session as fallback
waxtap download <url> --client web \
  --player-context-url http://127.0.0.1:4416/player-context \
  --session-url http://127.0.0.1:4416/session \
  --potoken-url http://127.0.0.1:4416/get_pot
```

Library callers get the same handoff without reimplementing the sidecar wire
format: `waxtap.NewSidecarPOTokenProvider`, `NewSidecarPlayerContextProvider`,
and `NewSidecarSessionProvider` build providers for `Options.POTokenProvider`,
`PlayerContextProvider`, and `SessionProvider`. Each takes a base URL or full
endpoint plus an optional `WithSidecarAPIKey`, and `ParseNetscapeCookies` loads a
static session from a browser `cookies.txt`. See `ExampleNewSidecarPOTokenProvider`
in [`example_test.go`](example_test.go).

Session adoption requires a single-client chain. Static adoption is also
available with `--visitor-data` and optional `--cookies`; visitor data is sent
verbatim, and login cookies are discarded. See [MAINTENANCE.md](MAINTENANCE.md)
for sidecar contracts and SABR diagnostics.

## Maintenance

Use `waxtap doctor` for a low-cost extraction, resolution, and byte-read health
check. Use `waxtap doctor --full` to verify complete delivery. The
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
