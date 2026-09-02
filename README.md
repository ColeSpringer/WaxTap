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
- Token-free VISIONOS is the default, with ANDROID_VR as fallback (since
  2026-08 the server refuses ANDROID_VR delivery of videos longer than about a
  minute). Full WEB audio is opt-in and needs an attested identity; forced iOS
  delivery is best-effort.
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
has `--help`, and `--json` is a stable scriptable contract (`schemaVersion` 3).

`--quiet` and `--verbose` are mutually exclusive; passing both is exit 2.

In `schemaVersion` 3 every failure, in every document, is an object:
`{"code": "needs-po-token", "message": "..."}`. Playlist and batch item errors
used to be bare strings, so only the top-level envelope could be switched on.
Documents also carry a `notes` array in the same `{code, detail}` shape as
`warnings`, holding the diagnostics that previously appeared only as `note:`
lines on stderr and vanished under `--quiet`. Batch items that failed or never
ran no longer name an output path they did not write, batch summary counts are
always present and include `total`, and `chapterCount` is omitted rather than
reported as 0 when chapters were never fetched.

A playlist run's exit code is the worst single item's classified code, so a
one-item playlist whose video needs a PO token exits 8 rather than a generic 1.

```sh
waxtap info <video-url>                         # metadata and best audio
waxtap info <video-url> --itag 251 --probe       # preview what a selection picks
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
codec. Album normalization applies one gain to every track: `--peak-mode cap`
leaves the true-peak limiter idle and reproduces the input's track-to-track
spacing exactly on a lossless target, at the cost of landing short; the default
`limit` reaches for the target and lets the per-track limiter compress that
spacing. Loudness uses EBU R128 (integrated LUFS, true peak dBTP, range LU).

### Notes

- `--channels mono|stereo|surround|any` picks a native layout, defaulting to
  stereo. It selects among a video's source streams, so it needs a URL input: on
  a local file or a directory it exits 2, unless `--downmix` is set too, where it
  names the fold target instead. (`normalize --album` and `--measure-loudness`
  reject it either way; neither writes a layout you chose.) `--downmix` allows
  surround-to-stereo/mono; it never upmixes, and on a file that already has the
  target layout it is a no-op that costs no re-encode. `--itag` names an exact
  encoding, so it overrides `--channels`; the run prints a note when the
  delivered layout is not the one asked for.
- `--format` takes `copy|flac|alac|wav|aiff|wavpack|ape|mp3|aac|he-aac|opus|vorbis`.
  Names are case-insensitive and trimmed, and a few spellings are aliases:
  `ogg` for vorbis, `m4a` for aac, `aif`/`aifc`/`afc` for aiff, `wv` for
  wavpack, `heaac` for he-aac, and `remux` for copy. `he-aac` encodes HE-AAC v1
  in `.m4a` at 64 kbps by default (a low-bitrate preset; `aac` stays the
  256 kbps AAC-LC one), and `--format aac` on a source that is already HE-AAC
  copies it under its own identity rather than re-encoding it to AAC-LC.
  WavPack, Monkey's Audio (APE), WMA, and Musepack (`.mpc`) files are also
  accepted as local inputs; WMA and Musepack are decode-only, so
  `--format copy` on one is refused.
- `--output-template` takes `{title}`, `{id}`, `{author}`, `{itag}`, `{ext}`,
  and `{index}`. `{index}` numbers playlist items and expands empty for a single
  video, taking one adjacent `-`, `_`, or space with it: `{index}-{title}.{ext}`
  gives `Song.opus`. It takes only one, so a placeholder padded on both sides
  (`{index} - {title}.{ext}`) leaves the other separator behind as `- Song.opus`.
- `--no-fallback` disables watch-page, WEB-context, and incomplete-download
  fallbacks. Results report the client that actually delivered. A delivery that
  came from the watch-page scrape rather than the player endpoint is labeled
  `via watch page` beside the client (`viaWatchPage: true` in `--json`), since
  the client name alone reads as a player delivery.
- Normalization applies one scalar gain. `--peak-mode cap` (the default) caps it
  so the true peak stays under -1.0 dBTP: transparent, but a source already
  peaking at 0 dBTP takes at most -1.0 dB whatever the target, so the miss can be
  10 LU or more (reported as the `loudness-target-missed` warning).
  `--peak-mode limit` applies the full gain and lets the true-peak limiter catch
  the overshoot, at the cost of transparency. The limiter gives back part of the
  gain it is handed, so `limit` measures its own output and corrects, re-encoding
  up to 4 times to land within 0.3 LU of the target, and reports
  `loudness-target-missed` when the limiter saturates before reaching it. Ordinary
  material costs one extra encode pass. `--album` applies one gain in a single
  pass and defaults to `limit`; correcting per track would undo the spacing album
  mode exists to keep. Its `cap` clamps the one gain by the album's least
  true-peak headroom rather than per track, so the loudest track sets the
  headroom for all of them and the miss can be large. The reporting threshold
  follows the mode's own promise: `limit` iterates onto the target, so it reports
  any miss past the 0.3 LU it converges to, while the single-pass policies
  (`cap`, and `--album` in either mode) report a miss over 1 LU, below which the
  clamp they can name is inside the noise of the encode.
- Decoding runs in float, so output depth follows the decoded stream: a lossy
  source gives 32-bit float WAV, 24-bit FLAC, and AIFF-C float rather than plain
  AIFF. That is lossless but larger, and some older DAWs and hardware players
  reject float WAV. `--bit-depth 16|24` forces integer output for
  wav/aiff/flac/alac/wavpack/ape; narrowing is dithered (TPDF), not truncated.
  The lossy formats encode in float and ignore it. WavPack and APE hold integer
  PCM only, so a float decode quantizes to 24 bits there by default, and both
  hold at most stereo: a surround source is refused rather than silently folded
  (pass `--downmix`). APE additionally caps at 24 bits and refuses a 32-bit
  integer source rather than narrowing it silently (pass `--bit-depth 24`).
- `--embed-thumbnail` writes the video's thumbnail as front cover art, and
  `--embed-metadata` writes title, artist, date, and chapters. Chapter marks
  follow any cut: shifted by the audio removed before them, dropped when their
  content was removed. Both flags are best-effort: a failure warns and still
  delivers valid audio. WebM cannot hold a
  picture, so a cover-art request remuxes Opus-in-WebM to Ogg-Opus (lossless) when
  the output extension allows it. The image comes from the largest rung YouTube
  lists, and WaxTap also probes the deterministic 1280x720 endpoints when that
  ladder falls short of them.
- `--cover-art square` crops the embedded picture to the release art. A music
  "Art Track" composites square cover art onto a 16:9 canvas and letterboxes that
  into a 4:3 variant, and both sets of bars are pixels rather than metadata, so
  every player that resizes without cropping ships them. `square` peels the
  uniform borders and center-crops what remains; it never scales, so the art keeps
  its proportions and an image already square within 1% is left untouched. The
  default, `--cover-art frame`, embeds the delivered bytes verbatim.
- Processing a local file (`transcode`, `normalize`, `cut`, `--album` included)
  carries the input's embedded metadata - tags, cover art, chapters, synced
  lyrics - onto the rewritten output. Anything the output format cannot hold is
  reported as the `tag-carry-incomplete` warning, never dropped silently.
  Chapter marks follow a cut the same way the embed flags do. Synced lyric
  lines follow a cut too, and a line whose timestamp lands in removed audio is
  dropped - including a line at 0:00 when the cut starts at zero - and reported
  through the same warning. Tags describing
  the source audio itself (ReplayGain, encoder stamps) carry only on a pure
  `--format copy` remux; a re-encode or cut invalidates them, so they are left
  off. WavPack and APE outputs are tagged the same way as every other format:
  their APEv2 block holds text tags and cover art (a Cover Art item), while
  chapters and synced lyrics have no APEv2 form and are reported as carry
  losses.
- SponsorBlock requests get a 10-second budget, so a `429` there fails fast and
  exits 5 (rate limited) rather than waiting out a `Retry-After` it cannot
  outlast. On a download, `--sponsorblock-on-error` decides whether that is fatal
  or delivers the audio uncut with a warning.
- Playlist downloads support `--concurrency`, pacing, attempt limits, collision
  policies, and yt-dlp-compatible `--download-archive` files.
- `--collision fail` (the default) and `auto-number` claim the output path with
  the publish itself, so two single-file runs writing the same path cannot both
  report success. Under `fail` the loser exits 2 with the existing-file message
  and the winner's file is intact. Under `auto-number` the loser renumbers
  instead, so N runs of one basename produce N distinct files; the numbering is
  not deterministic under concurrency, and racing runs can land `(2)` and `(3)`
  with `(1)` belonging to neither. `overwrite` opts into last-writer-wins. `skip` leaves
  the existing file alone and exits 0. On a single-file `transcode`, `normalize`,
  or `cut` it reports `{"skipped":"exists"}` with the path under `--json`, and
  prints that path under `--quiet` as a write does; `download` emits the same
  document without a path, and a directory batch reports the skip as one of its
  NDJSON item records.
  Two exceptions: `normalize --album` writes its tracks with a replacing rename,
  and on a filesystem without hard links (FAT, exFAT, some network shares) the
  publish degrades to a check-then-rename that a concurrent writer can still
  beat.
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
| 9 | network failure, including a proxy that is unreachable or rejects CONNECT, an unreachable sidecar, or an upstream HTTP error response |
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
pass `WithChannels(LayoutStereo)` to match the CLI. `WithSelector` and
`WithSourcePolicy` give `Info` and `InfoResult` the same selection `Download`
takes, so a caller can see what a given `--itag` or codec would pick without
fetching it; a selector that names its own layout beats `WithChannels`, which
only fills in one that named none. `Resolve` takes its selector as a parameter,
so only `WithSourcePolicy` applies there. `Client.Enumerate` expands a playlist or
channel URL with `Skip`/`Stop` predicates for an archive cursor, and
`WithFullMetadata()` adds publish date and chapters.

Bulk enumeration retires its guest identity and re-asks when YouTube's metadata
throttle starts refusing entries, because the refusal is worded exactly like a
removed video and only a fresh identity tells them apart. Entries left over
after the rotation budget report `ErrTemporarilyUnavailable`, which means retry
rather than skip.

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

`procs` bounds the concurrent audio-processing operations. Zero, the default,
follows `GOMAXPROCS`; a negative value disables the limit entirely. Both are
deliberate, so neither is rejected.

## PO tokens and WEB

VISIONOS and ANDROID_VR are token-free for public videos. WEB-family clients use URL-less
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

[MIT](LICENSE). Release archives also carry `THIRD-PARTY-NOTICES.md`, the
licenses of the modules compiled into the binary.
