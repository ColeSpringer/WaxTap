# WaxTap

An audio-focused YouTube downloader and audio processor for Go, shipped as both
a reusable library and a standalone CLI over one shared core.

WaxTap acquires audio (downloading from YouTube at the best available quality, or
taking a local file) and processes it: transcode, cut time ranges (for example,
SponsorBlock non-music segments), and measure or normalize loudness. The default
download does no re-encode; all processing is opt-in.

> **Status: under construction.** Phase 1 (foundation and contracts) is in place:
> module layout, the public facade API, the internal extraction/resolver seams,
> the error taxonomy, the HTTP client, and URL/ID parsing. Extraction, download,
> and processing land in subsequent phases.

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
| `format` | Audio-first `Format` model and selectors. |
| `sponsorblock` | SponsorBlock category vocabulary. |
| `potoken` | PO-token provider contract (caller-supplied in v1). |
| `waxerr` | The domain error taxonomy (one `errors.Is` source of truth). |
| `youtube` | YouTube extraction (volatile; exported for the facade, may churn). |
| `youtube/internal/resolver` | Cipher / base.js / stream-URL resolution. |
| `internal/httpx` | HTTP client: retry, backoff, Retry-After, per-host limiter. |
| `internal/tempfile` | Atomic temp-output staging + cleanup contract. |

## Requirements

- Go 1.26+
- `ffmpeg` / `ffprobe` on `PATH` (for transcode/cut/normalize; not needed for
  plain metadata or best-source downloads).

## Disclaimer

WaxTap is for personal and otherwise authorized use only. You are responsible for
complying with YouTube's Terms of Service and applicable law. v1 is public-only:
private, age-restricted, and login-gated videos are expected failures, not
something WaxTap promises to bypass.
