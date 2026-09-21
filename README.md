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
feed; a channel with no uploads feed reports the missing playlist by the
channel's own name, not by the `UU` id it resolved to. `cut`, `transcode`, and
`normalize` also take local files. Every command
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
waxtap info <video-url> --itag 251 --probe       # read the stream's own headers (--json marks it probed)
waxtap formats <video-url>                      # all audio formats (--json names the client they came from)
waxtap download <video-url> -o track            # keep source
waxtap download <video-url> --format flac -o track.flac
waxtap download <video-url> --sponsorblock --normalize --format mp3 -o track.mp3

waxtap transcode song.flac song.mp3
waxtap cut song.flac --cut-range 0:00-0:12 --cut-range 3:40-4:05 -o song-cut.flac
waxtap normalize song.wav --loudness-target -14 --format flac -o song.flac
waxtap normalize --album --format flac --dir ./normalized ./album/*.flac
waxtap split rip.flac --cue rip.cue -d ./tracks

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
codec, naming the container's encoder for an output named by extension alone,
so `implicit-lossy` is not raised.

Album normalization applies one gain to every track: `--peak-mode cap`
leaves the true-peak limiter idle and reproduces the input's track-to-track
spacing exactly, at the cost of landing short; the default `limit` reaches for
the target and lets the per-track limiter compress that spacing. Every track is
measured at the width its own encode delivers, so a lossy target's fold of a
surround master is in the figures the gain comes from, and the album figure is
EBU R128's gates run over every track's blocks at once rather than over a
concatenation: a mono track is measured as mono, not as the dual-mono a stereo
timeline would make of it, and a track whose channel positions differ from its
neighbours' is measured as it is. A track that does not read to the length its
container states exactly is refused, naming the file: every track shares one
gain, and one derived from part of a track describes nothing. Loudness uses
EBU R128 (integrated LUFS, true peak dBTP, range LU).

### Notes

- `--cut-range START-END` removes a span, repeatable or several comma-separated
  in one flag. Times are `[HH:]MM:SS[.frac]`, a Go duration (`90s`, `1m30s`), or
  bare seconds, never signed. `--cut-mode smart|copy|accurate` picks how the
  cut is rendered: `smart` (the default) stream-copies when it can, `copy`
  refuses anything that would need a decode, `accurate` always decodes. A
  packet-level copy (`smart` on an Opus or AAC source, and `copy`) keeps whole
  packets: the first cut point and the last are exact, and every interior join
  moves inward to the packet grid, so up to one frame (20 ms Opus, 21 to 23 ms
  AAC) of wanted audio is missing at each join and nothing from a removed span
  is delivered; the run reports it as `cut-snapped`, and `--json` carries
  `cutMode`, `cutSnaps`, and `cutSnapMaxMs`. `copy-exact` keeps the copy and
  makes each interior tail exact, with the decoder converged across the join,
  through per-packet trims that only `.mka`/`.mkv`/`.webm` can carry (Firefox
  rejects such a file; other players honour it); the heads still land within one
  frame. `--crossfade` blends each join and forces a decode; `accurate` and
  `--crossfade` re-encode, so they need `--format` or an output extension that
  names a format. Ranges are clamped to the
  media and merged; ranges that fall entirely outside it are a request error.
- `split` cuts a single-file rip into one file per track at the sheet's
  `INDEX 01` positions, exact to the CD frame (1/75 s). Pieces are named
  `NN - Title.ext` and tagged from the sheet (title, performer, track numbers,
  disc title as album, `REM DATE`/`GENRE`, `CATALOG`, `ISRC`), with the rip's
  own tags and cover art carried underneath. Audio before the first track's
  `INDEX 01` becomes `00 - Hidden Track` rather than being folded into track 1
  or dropped. `--cue` defaults to a sheet beside the rip with the rip's stem. A
  split always decodes, so `--format` names the encoder; it is inferred from the
  rip's extension only when that is a lossless one. A sheet indexing several
  files is refused: its tracks are already separate.
- `--channels mono|stereo|surround|any` picks a native layout, defaulting to
  stereo. It selects among a video's source streams, so it needs a URL input: on
  a local file or a directory it exits 2, unless `--downmix` is set too, where it
  names the fold target instead. (`normalize --album` and `--measure-loudness`
  reject it either way; neither writes a layout you chose.) `--downmix` allows
  surround-to-stereo/mono; it never upmixes, and on a file that already has the
  target layout it is a no-op that costs no re-encode. `--itag` names an exact
  encoding, so it overrides `--channels`; the run prints a note when the
  delivered layout is not the one asked for.
- An output extension names a container. Without `--format`, a format-named
  extension (`.flac`, `.mp3`, `.opus`, `.wav`, `.aiff`, `.wv`, `.ape`) selects
  that format; a container that holds several codecs (`.ogg`/`.oga`,
  `.mka`/`.mkv`, `.webm`, `.mp4`/`.m4a`/`.m4b`, `.aac`) keeps the source codec
  when it can carry it (a copy, or under `normalize --peak-mode cap` the Opus
  header gain) and otherwise runs the container's usual encoder (`.ogg` Vorbis,
  `.mka`/`.webm` Opus, `.mp4` AAC), reported as `implicit-lossy`. A URL's codec
  is checked after the download, so `transcode <url> out.mka` copies an Opus
  stream, cuts it at the packet grid like `cut` does when a cut is asked for,
  and `out.m4a` encodes it. `--format ogg` still means Vorbis.
- `--format` takes `copy|flac|alac|wav|aiff|wavpack|ape|mp3|aac|he-aac|opus|vorbis`.
  Names are case-insensitive and trimmed, and a few spellings are aliases:
  `ogg` for vorbis, `m4a` for aac, `aif`/`aifc`/`afc` for aiff, `wv` for
  wavpack, `heaac` for he-aac, and `remux` for copy. `he-aac` encodes HE-AAC v1
  in `.m4a` at 64 kbps by default (a low-bitrate preset; `aac` stays the
  256 kbps AAC-LC one), and `--format aac` on a source that is already HE-AAC
  copies it under its own identity rather than re-encoding it to AAC-LC.
  WavPack, Monkey's Audio (APE), WMA (versions 1 and 2, Pro, Voice, and
  Lossless), and Musepack (`.mpc`) files are also accepted as local inputs, as
  are WAV, AIFF-C, and MP4/MOV files carrying G.711 or IMA ADPCM audio or MP3
  frames, Microsoft ADPCM in WAV and MP4/MOV, and uncompressed PCM in
  MP4/MOV. WMA, Musepack, G.711, and ADPCM are decode-only, so
  `--format copy` on one is refused; an MP3 carried in a WAV or MP4 copies
  out to a bare `.mp3`. Chapters (ASF markers, SV8 chapter packets) carry
  like any other input's. `--format copy` is refused on WAV and AIFF (PCM
  belongs to its container), and a same-container PCM request is delivered as a
  byte copy with `same-format-copied`; `--bit-depth`, `--downmix`, and
  `--force` still re-encode. `--bitrate` is what the encoder is asked for; each
  encoder has its own range and grid, and a rate it cannot use exactly is
  snapped to the nearest it supports and reported as `bitrate-adjusted`: MP3
  keeps to its CBR table (32 to 320 kbps at 32, 44.1, and 48 kHz; 8 to
  160 kbps below), Opus takes 6 to 510 kbps (a lower request is refused), AAC
  and HE-AAC 8 kbps per channel and up. Vorbis is quality-driven and ignores
  `--bitrate` (noted as `flag-inert`).
- `--output-template` takes `{title}`, `{id}`, `{author}`, `{itag}`, `{ext}`,
  and `{index}`. `{index}` numbers playlist items and expands empty for a single
  video, taking one adjacent `-`, `_`, or space with it: `{index}-{title}.{ext}`
  gives `Song.opus`. It takes only one, so a placeholder padded on both sides
  (`{index} - {title}.{ext}`) leaves the other separator behind as `- Song.opus`.
- `--no-fallback` disables watch-page, WEB-context, and incomplete-download
  fallbacks. Results report the client that actually delivered: the attested
  player-context path reports its client as `WEB_CONTEXT`, while the session
  and static adoption paths report `WEB`. A delivery that
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
  pass and defaults to `limit`, and takes files, not a directory; correcting per track would undo the spacing album
  mode exists to keep. Its `cap` clamps the one gain by the album's least
  true-peak headroom rather than per track, so the loudest track sets the
  headroom for all of them and the miss can be large. The reporting threshold
  follows the mode's own promise: `limit` iterates onto the target, so it reports
  any miss past the 0.3 LU it converges to, while the single-pass policies
  (`cap`, and `--album` in either mode) report a miss over 1 LU, below which the
  clamp they can name is inside the noise of the encode. `normalize` re-encodes
  whatever codec the output keeps or names, so a lossy source kept by its
  extension (an MP3 to `.mp3`, an AAC to `.mka`) pays a second lossy generation;
  the Opus header-gain path is the one normalize that does not.
- On an Opus source that stays Opus, `--peak-mode cap` writes the gain into the
  Opus header (the `OpusHead` output gain, which every compliant player applies)
  and copies the packets untouched, in whichever container the output names
  (`.opus`, `.ogg`, `.mka`, `.webm`), so
  the run costs no generation of loss; `--json` reports `loudness.headerGain`
  beside `loudness.gainDb`, and the result is a remux (`transcoded: false`),
  which like every other local copy omits `outputFormat`: the delivered codec
  is the source's, under `sourceFormat`. `loudness.gainDb` is the amount the
  header moved by, so a source whose head already stated a gain keeps it and
  the file leaves with the sum.
  `--peak-mode limit`, an explicit `--bitrate`, a cut, or a downmix re-encode as
  before, and `--album` takes the same path under `cap` when every member is
  Opus. The ReplayGain and R128 tags are dropped on that path: the header now
  carries the gain they would restate.
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
  the output extension allows it, reported as the `cover-art-remuxed` note;
  `--json` then names the Ogg container under `outputFormat`, since that is the
  file on disk. The image comes from the largest rung YouTube
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
  reported as the `tag-carry-incomplete` warning, never dropped silently; the
  summary prints a `Metadata:` receipt (a column per track under `--album`)
  and `--json` itemizes the carry as `tagCarry`. Chapter marks follow a cut the
  same way the embed flags do. Synced lyric lines follow a cut too, whether
  they are stored as a synced set or as LRC text in the plain lyrics field: a
  line whose timestamp lands in removed audio is dropped - including a line at
  0:00 when the cut starts at zero - and reported through the same warning.
  Prose in that field is carried verbatim, since it has no timestamps to move. Tags describing
  the source audio itself (ReplayGain, encoder stamps) carry only on a pure
  `--format copy` remux; a re-encode or cut invalidates them, so they are left
  off. WavPack and APE outputs are tagged the same way as every other format:
  their APEv2 block holds text tags and cover art (a Cover Art item), while
  chapters and synced lyrics have no APEv2 form and are reported as carry
  losses.
- SponsorBlock cuts on a download take the same packet-level path, so the same
  snap and `cut-snapped` warning apply.
- The `sponsorblock` preview fetches the video's length and marks segments the
  database holds past its end, leaving them out of the removal total. `--json`
  carries `durationSeconds` and a per-segment `pastEnd`; a length it could not
  fetch omits `durationSeconds`, counts every segment at its word, and is
  reported as the `length-unchecked` note.
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
  beat. `split` refuses `--collision skip`: a partly written set is worse than
  a refusal.
- `waxtap cache dir` and `waxtap cache clean` manage the persistent player-JS
  cache; `--no-cache` disables it. `cache clean` removes only what WaxTap wrote
  (the entry directories it marks with a `CACHEDIR.TAG` naming WaxTap as the
  writer, which is read rather than taken on the file's name) and then the
  cache directory itself when that empties it, so a mistyped `--cache-dir`
  cannot cost you files: a path that is not a directory is a usage error, and
  one holding nothing of WaxTap's is left alone. `cache dir --json` reports
  `exists` for the path and `populated` for a WaxTap cache inside it.

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
| 9 | network failure, including a proxy that is unreachable, never answers, or rejects CONNECT, an unreachable sidecar, or an upstream HTTP error response |
| 10 | local I/O failure |
| 130 | canceled by SIGINT or SIGTERM |
| 141 | the stdout reader closed the pipe (`download -o -`); the shell reports the SIGPIPE, WaxTap prints nothing |

Malformed targets exit 2; a well-formed but nonexistent or private video can
only be classified after a network request and exits 3.

### Warnings and notes

A run that succeeds can still have something to say. Warnings are conditions
the library observed; notes are decisions the command line made. Both carry a
stable code and a human detail, both go to stderr in human mode, and both
appear in `--json` as `warnings[]` and `notes[]`.

| Warning | Meaning |
|---|---|
| `proceed-uncut` | the SponsorBlock fetch failed; the audio was delivered uncut |
| `fallback-profile` | a fallback client profile delivered the stream |
| `url-re-resolved` | an expired stream URL was re-resolved mid-transfer |
| `playlist-entry-failed` | one playlist entry failed; the others were returned |
| `rate-limited-retried` | a request was retried after a 429 |
| `sponsorblock-empty` | SponsorBlock matched no segments |
| `ranges-empty` | every SponsorBlock segment fell outside the media |
| `throttled` | a host rate-limited the run; further requests to it are held back for the stated wait |
| `web-context-fallback` | the WEB player context failed; the configured chain took over |
| `incomplete-fallback` | a client returned an incomplete stream; another client was tried |
| `web-context-retry` | the WEB player context was capped; it was retried once with a fresh one |
| `metadata-embed-failed` | `--embed-thumbnail`/`--embed-metadata` could not write everything asked for |
| `loudness-target-missed` | the loudness target was not reached; the detail says what held it back |
| `session-rotated` | a fresh guest session replaced one whose URLs the server kept rejecting |
| `tag-carry-incomplete` | some of the input's embedded metadata did not reach the output |
| `implicit-downmix` | the encoder folded channels the request never asked to lose |
| `output-clipping` | the delivered file's level is past full scale |
| `implicit-lossy` | the output container could not carry the source codec, so a lossy encoder the request never named ran (a `cut`, or an extension-named output with no `--format`) |
| `input-damage` | the decoder worked around problems in the input; the readable audio was delivered |
| `loudness-unmeasurable` | an integrated loudness came back non-finite; the detail names the side and the cause |
| `source-policy-unmatched` | `--source-policy prefer:<codec>` named a family this video does not offer |
| `empty-input` | the input's audio track holds no frames |
| `input-note` | the engine's own remarks on an undamaged input |
| `cut-snapped` | a packet-level copy cut moved interior joins to the packet grid; the detail counts them and names the largest move |
| `bitrate-adjusted` | the encoder used the nearest bit rate it supports; the detail names the requested and the delivered rate |
| `watch-page-no-token` | a WEB run fell back to the watch page, so the PO token it minted was never exercised |
| `gapless-dropped` | a copy into raw ADTS (`.aac`) delivered the source's encoder delay and padding as audio, since ADTS states no gapless trim and no length |

| Note | Meaning |
|---|---|
| `alac-mp4-container` | `.alac` output is an MP4 container; `.m4a` is the conventional name |
| `archive-not-recorded` | a finished file was not added to the download archive; a re-run fetches it again |
| `channels-ignored` | `--itag` names an exact encoding, so `--channels` did not affect selection |
| `channels-unavailable` | the requested layout was not available; the delivered one is named |
| `concurrency-clamped` | `--concurrency` exceeded the maximum and was clamped |
| `container-ext-mismatch` | the output extension does not match the source container, which was copied unchanged |
| `cover-art-remuxed` | a source whose container cannot hold a picture was remuxed into its codec's own so the cover art could be embedded; packets unchanged |
| `cue-file-mismatch` | the CUE sheet names another file than the rip being split |
| `doctor-caveat` | a `doctor` check passed with a caveat; the detail says what it did not prove |
| `enumeration-error` | a playlist page failed to enumerate; the run continued |
| `flag-inert` | a flag had no effect on this run |
| `forced-client-risky` | a forced `--client` is known to deliver unreliably |
| `kept-output` | a finished file was kept after a failure elsewhere in the run |
| `length-unchecked` | the SponsorBlock preview could not fetch the video's length, so segments were not checked against it |
| `playlist-ignored` | a playlist URL was passed to a video command; the video was used |
| `probe-skipped` | `--probe` read nothing: the selected stream is SABR-only |
| `same-format-copied` | the input is already the target format and was copied; `--force` re-encodes |
| `selection-unmatched` | no audio format matched the requested selection |
| `sidecar-write-failed` | the `--write-info-json` sidecar could not be written |
| `unaltered-copy` | the output is a byte-for-byte copy of the source |
| `watch-page-formats` | the format list came from the watch-page fallback, which needs no PO token |
| `web-sources` | a single WEB identity source is configured; it fires before info/formats and after a download a second source could have helped, including a WEB delivery that fell back to the watch page |

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
`WithFullMetadata()` adds publish date and chapters to `Info`, or through
`EnrichOptions` to each enriched entry. `Client.PlanSplit` reads a CUE sheet
against a local rip and reports where the pieces fall, and `Client.Split` writes
them, tagged from the sheet; the `split` subcommand is that pair.

Bulk enumeration retires its guest identity and re-asks when YouTube's metadata
throttle starts refusing entries, because the refusal is worded exactly like a
removed video and only a fresh identity tells them apart. Entries left over
after the rotation budget report `ErrTemporarilyUnavailable`, which means retry
rather than skip. `Enrich` attaches what each call fetched as
`PlaylistEntry.Video` and overlays the entry's title, author, and duration with
it, keeping a listing value where the fetch had none, `MaxEnrich` caps the pass
at the first n fetchable entries so a per-entry budget is spent inside the loop
that rotates, `EnrichOptions` pass `WithFullMetadata()` or `WithNoFallback()` to
each call, and a failed entry is an `EnrichError` naming it. Entries the listing
marked live or upcoming (`PlaylistEntry.LiveStatus`) are passed over: `Info`
refuses them, so the call would spend a request, and a budget slot, on what the
badge already said. `--list` shows the marker in the duration column and as
`liveStatus` in `--json`.

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
A file named by `--config` or `WAXTAP_CONFIG` must exist; only the default
location is optional. Unknown JSON keys and malformed environment values are
errors.

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
| `sidecarTimeoutSeconds` | `WAXTAP_SIDECAR_TIMEOUT` | - |
| `sponsorBlockTimeoutSeconds` | `WAXTAP_SPONSORBLOCK_TIMEOUT` | - |
| `chunkTimeoutSeconds` | `WAXTAP_CHUNK_TIMEOUT` | - |

`tempDir` names where downloaded sources and processed downloads are staged
before delivery; a local file is processed beside its own output, so the
setting does not apply to it. A path that exists must be a writable directory,
checked at startup; a missing one is created on first use, so a command that
stages nothing creates nothing.

Timeouts default to 45 s extraction, 30 s resolve, 60 s web context, 60 s per
sidecar request, 10 s SponsorBlock, 120 s per chunk. The web-context timeout
bounds one attested handoff as a whole, a `/player-context` call or a `/session`
resolution together with the wait the sidecar asks for and the one retry; the
sidecar timeout bounds each request inside it. Setting
`sidecarTimeoutSeconds` to 0 selects its default rather than "no deadline",
unlike the other timeout keys: the handoff budget already bounds the call, and
the retry needs a per-request bound to be reachable.

A proxy dial is bounded at 10 s and not retried, so a proxy that never answers
fails inside any budget with the proxy named rather than consuming the whole
extraction budget and reporting a bare timeout. A proxy the first request
cannot dial at all ends the run there, with exit 9, rather than paying a
second dial to learn the same thing.

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
chain can use the adopted session.

A refusal the sidecar codes is read: `video-unavailable` is the video's
playability verdict (exit 3, skip-class, the chain still tries the native
clients), and a refusal that states a wait (`Retry-After` or
`retry_after_seconds`) is retried once after it, up to 60 s. A `/session` or
`/player-context` that exports `user_agent` and `client_version` has WaxTap's
WEB requests carry that browser's identity. A context that carries the video's
channel, description, thumbnail ladder, live flags, and publish date fills
`Result.Metadata`,
`--write-info-json`, and the cover-art ladder on that path as every other path
does.

Static adoption is also available with
`--visitor-data` and optional `--cookies`. Library callers get the same handoff
via the `NewSidecar*` providers, each taking a base URL or full endpoint plus an
optional `WithSidecarAPIKey`; `PingSidecar` asks the daemon behind such a URL
for its health; `ParseNetscapeCookies` loads a static session from a browser
`cookies.txt`. See [MAINTENANCE.md](MAINTENANCE.md) for sidecar
contracts and SABR diagnostics.

## Maintenance

`waxtap doctor` runs a low-cost extraction, resolution, and byte-read health
check; `waxtap doctor --full` verifies complete delivery. A `--session-url`
needs a uniform client chain: a single `--client` of any name, or a
single-client `--profile-override`, since an adopted session cannot span the
several clients the default chain tries. With
sidecar URLs configured, the daemon behind them is asked for its health first
(WaxSeal's `/ping`, one round trip, reported with its reason: `ok`,
`no-session`, `busy`, or `probe-failed`, and whether the daemon is keyed), then
each endpoint is probed once (session, PO token, player-context), so a cold
daemon's first-call cost and any refusal code are visible; the `--json` ping
entry carries a one-word `status`: healthy, answered, not-offered, or failed; the token and context probe latencies include WaxSeal's separation
waits, which is the cost a first download pays, not a relaunch. A probe that
relays the video's own playability verdict still counts as a healthy sidecar. The
[maintenance runbook](MAINTENANCE.md) covers dumps, profile refreshes, cipher
failures, SABR changes, fixtures, and releases. Work cut from a change is
tracked in [docs/deferred-work.md](docs/deferred-work.md), and what WaxTap
wants from WaxFlow, WaxLabel, and WaxSeal in
[docs/upstream-requests.md](docs/upstream-requests.md).

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
