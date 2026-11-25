# WaxTap

WaxTap downloads YouTube audio and processes audio files: transcode, cut, drop
SponsorBlock segments, split by CUE sheet, and measure or normalize loudness.
It is a Go library and the `waxtap` CLI, one static binary in pure Go with no
yt-dlp or ffmpeg behind it.

A plain download keeps the source stream untouched. Processing is opt-in.

> WaxTap targets public videos. Private, age-restricted, and login-gated videos
> are expected failures.

## Install

```sh
go install github.com/colespringer/waxtap/v3/cmd/waxtap@latest   # CLI
go get github.com/colespringer/waxtap/v3                         # library
```

Both need Go 1.26+. [Release archives](https://github.com/colespringer/waxtap/releases/latest)
hold Linux, macOS, and Windows binaries for amd64 and arm64. Unsigned macOS
binaries may need `xattr -d com.apple.quarantine /path/to/waxtap`; Windows may
prompt SmartScreen.

## Usage

```sh
waxtap info <url>                               # metadata and best audio
waxtap formats <url>                            # every audio format
waxtap download <url> -o track                  # keep the source stream
waxtap download <url> --format flac -o track.flac
waxtap download <url> --sponsorblock --normalize --format mp3 -o track.mp3
waxtap download <url> --embed-metadata --embed-thumbnail
waxtap download <playlist-url> -d ./music --download-archive archive.txt
waxtap download <channel-url> --list            # list entries, no download

waxtap transcode song.flac song.mp3
waxtap cut song.flac --cut-range 0:00-0:12 --cut-range 3:40-4:05 -o song-cut.flac
waxtap normalize song.wav --loudness-target -14 --format flac -o song.flac
waxtap normalize --album --format flac --dir ./normalized ./album/*.flac
waxtap split rip.flac --cue rip.cue -d ./tracks

waxtap sponsorblock <url>                       # preview segments
waxtap doctor                                   # check that extraction works
```

Commands that fetch take a YouTube URL or a bare ID; `download` also takes
playlists and channels. `transcode`, `cut`, and `normalize` take a URL or a
local file, `transcode` and `normalize` also a directory (`-r` recurses), and
`split` a local rip only. `--help` on any command is the full flag reference.

- Formats: `--format` takes `copy`, `flac`, `alac`, `wav`, `aiff`, `wavpack`,
  `ape`, `mp3`, `aac`, `he-aac`, `opus`, or `vorbis`. Without it the output
  extension picks the format, and a container that holds several codecs
  (`.mka`, `.m4a`, `.ogg`) keeps the source codec when it can. YouTube's audio
  is lossy, so FLAC from a download is a larger file, not a better one. A
  source already in the target format is copied; `transcode --force` re-encodes
  it. Local inputs can be any of those formats, plus WMA and Musepack, which
  are decode-only.
- Cuts: `--cut-range START-END` removes a span and repeats. `--cut-mode smart`,
  the default, copies packets when the codec allows it (Opus, AAC), which costs
  up to one frame, about 20 ms, at each interior join. `accurate` decodes for
  exact cuts, `copy` refuses to decode, and `copy-exact` makes the tail of each
  join exact on a `.mka` or `.webm` output. `--crossfade` blends each join.
- Loudness: EBU R128. `--normalize` on a download, or `waxtap normalize`, aims
  for -14 LUFS (`--loudness-target`). `--peak-mode cap`, the default, holds the
  true peak under -1 dBTP without limiting, so a loud master can land short of
  the target; `limit` reaches the target through a true-peak limiter. `--album`
  gives every track the same gain and defaults to `limit`. An Opus source kept
  as Opus under `cap` gets the gain in its header and no re-encode.
  `--measure-loudness` reports without changing the audio.
- Channels: `--channels mono|stereo|surround|any` picks among a video's streams
  and defaults to stereo. `--downmix` folds surround down; nothing is upmixed.
- Tags and cover art: `--embed-metadata` writes title, artist, album, year,
  copyright, and chapters. `--embed-thumbnail` adds the thumbnail as cover
  art, and `--cover-art square` crops the bars off an Art Track's release art.
  Tags come from the video and its page; nothing is looked up elsewhere.
  Processing a local file carries its tags, cover art, chapters, and lyrics to
  the output and reports whatever the new format cannot hold.
- Playlists and channels: `--download-archive` (yt-dlp compatible) skips what
  an earlier run fetched. `--concurrency`, `--sleep-interval`, `--max-items`,
  and `--max-downloads` pace and bound a run. `--output-template` names files
  from `{title}`, `{id}`, `{author}`, `{itag}`, `{ext}`, and `{index}`.
  `--collision fail|overwrite|auto-number|skip` settles an existing file and
  defaults to `fail`.
- SponsorBlock: `--sponsorblock` removes `music_offtopic` segments. Other
  categories attach with `=`, as in `--sponsorblock=sponsor,selfpromo`. A
  failed lookup delivers the audio uncut with a warning;
  `--sponsorblock-on-error fail` makes it an error.
- Scripting: `--json` is a stable contract (`schemaVersion` 3) and `--quiet`
  prints only the output path. Failures are `{"code", "message"}` objects, and
  a run that succeeds reports what it had to say as `warnings` and `notes`,
  whose codes are listed in
  [docs/warnings-and-notes.md](docs/warnings-and-notes.md). Exit codes are
  [below](#exit-codes).

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

The root `waxtap` package is the stable API. `Download`, `Stream`, `Info`,
`Enumerate`, and `DownloadPlaylist` work on YouTube; `Process`, `ProcessAlbum`,
`Measure`, and `Split` work on local files.

- A `Download` with no `ProcessSpec` delivers the source stream byte for byte.
- Selection defaults to stereo, as the CLI's does. To let a surround track
  win, set `Request.Audio` to `BestAudio().WithChannels(LayoutAny)`, or pass
  `WithChannels(LayoutAny)` to `Info`.
- Availability failures (`ErrVideoUnavailable`, `ErrAgeRestricted`,
  `ErrGeoBlocked`, `ErrLiveContent`, and siblings) are typed sentinels a feed
  consumer should skip. `ErrTemporarilyUnavailable` means retry.

The [package documentation](https://pkg.go.dev/github.com/colespringer/waxtap/v3)
has the full error taxonomy, and [`example_test.go`](example_test.go) has
runnable examples.

## Configuration

Precedence, highest first: flag, `WAXTAP_*` environment variable, JSON config
file, default. The file is `config.json` under `os.UserConfigDir()/waxtap`, or
the path in `--config` or `WAXTAP_CONFIG`. Unknown keys and malformed values
are errors.

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
| `sidecarTimeoutSeconds` | `WAXTAP_SIDECAR_TIMEOUT` | - |
| `sponsorBlockTimeoutSeconds` | `WAXTAP_SPONSORBLOCK_TIMEOUT` | - |
| `stallTimeoutSeconds` | `WAXTAP_STALL_TIMEOUT` | - |

Timeouts default to 45 s extraction, 30 s resolve, 60 s web context, 60 s per
sidecar request, 10 s SponsorBlock, and a 30 s stall. The stall timeout bounds
the wait for the next byte, not the whole transfer, so a slow link finishes and
a hung connection fails fast. A timeout of 0 means no deadline, except the
sidecar and stall timeouts, where 0 selects the default. `procs` caps
concurrent audio operations; 0 follows `GOMAXPROCS` and a negative value lifts
the cap.

## WEB client and PO tokens

The default clients, VISIONOS with ANDROID_VR as fallback, need no token for
public videos. WEB is opt-in and needs three things together: a PO-token
provider, an attested identity (a `/player-context` handoff or an adopted
`/session`), and one egress IP shared by the attesting service and the
download. A PO token alone does not lift WEB's preview cap.

```sh
waxtap download <url> --client web \
  --player-context-url http://127.0.0.1:4416/player-context \
  --session-url http://127.0.0.1:4416/session \
  --potoken-url http://127.0.0.1:4416/get_pot
```

Library callers wire the same sidecar through `NewSidecarPOTokenProvider`,
`NewSidecarPlayerContextProvider`, and `NewSidecarSessionProvider`.
[MAINTENANCE.md](MAINTENANCE.md) has the sidecar contracts.

## When it breaks

YouTube changes without notice. `waxtap doctor` probes any configured sidecars,
then extracts a known video, resolves its audio, and reads a few KiB; `--full`
downloads a whole track instead. [MAINTENANCE.md](MAINTENANCE.md) is the
runbook for breakage, profile refreshes, and releases. Deferred work is tracked
in [docs/deferred-work.md](docs/deferred-work.md), and asks of WaxFlow,
WaxLabel, and WaxSeal in [docs/upstream-requests.md](docs/upstream-requests.md).

## Exit codes

`waxtap exit-codes` prints these in more detail. The same class is
`error.code` in `--json`. A playlist or batch run exits with the highest code
among its items.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unclassified error |
| 2 | invalid request or config, unsupported input, or a requested format that is unavailable |
| 3 | video or playlist unavailable or restricted: private, age-restricted, members-only, geo-blocked, removed, login required, live or upcoming, or no audio |
| 4 | extraction, cipher, or playlist parsing failure; WaxTap may need an update |
| 5 | rate limited |
| 6 | retired, never returned |
| 7 | incomplete stream or expired stream URL |
| 8 | PO token required: none configured, mint failed, or rejected |
| 9 | network failure: a connection, proxy, or sidecar error, or an upstream HTTP error response |
| 10 | local I/O failure |
| 130 | canceled by SIGINT or SIGTERM |
| 141 | the reader of `download -o -` closed the pipe (SIGPIPE) |

## Acknowledgements

WaxTap was influenced by [kkdai/youtube](https://github.com/kkdai/youtube) and
[yt-dlp](https://github.com/yt-dlp/yt-dlp), but ships no code from either.

## Disclaimer

WaxTap is for personal and otherwise authorized use. You are responsible for
complying with YouTube's Terms of Service and applicable law.

## License

[MIT](LICENSE). Release archives also carry `THIRD-PARTY-NOTICES.md`, the
licenses of the modules compiled into the binary.
