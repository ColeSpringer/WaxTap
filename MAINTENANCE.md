# Maintaining WaxTap

WaxTap extracts audio from YouTube, whose player and anti-bot surfaces change
without notice. This document is the playbook for when something breaks, plus the
reference for the knobs that let you respond without rebuilding.

## Quick response: YouTube broke extraction

1. **Confirm it.** Run the health check:
   ```
   waxtap doctor            # extract + resolve + small byte read (cheap)
   waxtap doctor --full     # also download a whole track
   waxtap doctor --video-id <id-or-url>   # check a specific video
   ```
   `doctor` tries a list of known-good videos, so one deleted video does not
   decide the result. Exit code `4` means an extraction/cipher failure: the class
   that usually requires a WaxTap code or profile update. Exit `3` means
   availability/restriction, and exit `5` means rate limiting; both are often
   environmental, especially from datacenter IPs.

2. **Capture what YouTube returned.** Set the dump directory and reproduce:
   ```
   WAXTAP_DUMP_DIR=./dump waxtap info <url>
   ```
   On an extraction failure, WaxTap writes the raw player response(s) it could not
   use to `./dump/<timestamp>-playerresponse-<client>-<videoID>.json` (and the
   watch page on a watch-page parse failure). These are the ground truth for
   diagnosis. They are diagnostic only and never change WaxTap's behavior.

3. **Capture a fresh base.js** (the cipher source). WaxTap already persists it in
   the on-disk cache (see below); you can also fetch it directly. Find the current
   player URL from any watch/embed page (`/s/player/<hash>/.../base.js`) and:
   ```
   curl -s 'https://www.youtube.com/s/player/<hash>/player_ias.vflset/en_US/base.js' -o base.real.js
   ```
   `*.real.js` and `testdata/real/` are git-ignored on purpose (licensing): use
   them locally to derive an **authored, minimized** fixture, never commit the raw
   file.

4. **Fix the smallest surface.** Breakage usually lands in one of three files:
   | Symptom | File |
   |---|---|
   | Bot wall / playability `ERROR` / stale client version | `youtube/profile.go` (client versions, user agents, device fingerprints, `RequiresPOToken`) |
   | Signature / `n`-parameter solve fails (exit 4, `ErrCipherSolve`) | `youtube/internal/resolver/cipher.go` (the cipher/`n` locators) |
   | Player response shape changed (parse/format extraction) | `youtube/playerresponse.go` |

   Reproduce against your captured fixture, adjust, and run the checks below.
   If the recovery path or runtime knobs changed, update this file in the same
   patch.

## Verifying a fix

- **Unit (offline), authored fixtures:** `go test ./...`
- **Race:** `go test -race ./...`
- **Live (network), build-tagged:** `go test -tags=integration ./...`
  Live tests hit real YouTube and may be rate-limited or bot-walled from
  datacenter/CI IPs; a skip there is expected, a cipher failure is not.
- **Cross-compile:** `GOOS=windows GOARCH=amd64 go build ./...` and
  `GOOS=darwin GOARCH=arm64 go build ./...`

CI (`.github/workflows/ci.yml`) runs gofmt, vet, build, race tests, and
cross-compile on every push/PR. The daily `doctor` cron
(`.github/workflows/doctor-cron.yml`) is an early-warning probe: it only fails the
job on an extraction/cipher failure (exit 4). Availability and rate-limit
failures warn and keep the job green because GitHub runner IPs are frequently
throttled by YouTube.

## Client-profile override files

When YouTube only needs a **client version or user-agent bump**, you do not need a
rebuild. Point WaxTap at a JSON file that replaces the built-in client strategy
chain:

```
waxtap --profile-override ./profiles.json info <url>
# or: WAXTAP_PROFILE_OVERRIDE=./profiles.json, or "profileOverridePath" in config.json
```

The file declares the full ordered chain; the first client that works wins. An
override replaces the built-ins, so include every fallback you still want. This
template mirrors the current defaults in `youtube/profile.go`:

```json
{
  "profiles": [
    {
      "name": "ANDROID_VR",
      "innerTubeName": "ANDROID_VR",
      "innerTubeId": 28,
      "version": "1.65.10",
      "userAgent": "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip",
      "deviceMake": "Oculus",
      "deviceModel": "Quest 3",
      "osName": "Android",
      "osVersion": "12L",
      "androidSdkVersion": 32,
      "requiresPoToken": "none",
      "supportsPlaylists": false
    },
    {
      "name": "IOS",
      "innerTubeName": "IOS",
      "innerTubeId": 5,
      "version": "19.45.4",
      "userAgent": "com.google.ios.youtube/19.45.4 (iPhone16,2; U; CPU iOS 18_1_0 like Mac OS X;)",
      "deviceModel": "iPhone16,2",
      "requiresPoToken": "none",
      "supportsPlaylists": false
    },
    {
      "name": "WEB_EMBEDDED_PLAYER",
      "innerTubeName": "WEB_EMBEDDED_PLAYER",
      "innerTubeId": 56,
      "version": "1.20250310.01.00",
      "userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
      "requiresPoToken": "none",
      "supportsPlaylists": false
    },
    {
      "name": "WEB",
      "innerTubeName": "WEB",
      "innerTubeId": 1,
      "version": "2.20250310.01.00",
      "userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
      "requiresPoToken": "gvs",
      "supportsPlaylists": true
    }
  ]
}
```

Notes:

- `requiresPoToken` is a scope name: `none`, `gvs`, `player`, or `subtitles`.
- Headers such as `X-Youtube-Client-Name` are derived from the scalar fields. Do
  not add a separate header map to the JSON.
- The loader is strict: unknown keys, trailing data after the JSON document, an
  empty list, or a profile missing `name`/`innerTubeName`/`version`/a positive
  `innerTubeId` is a hard error. (`innerTubeId` drives `X-Youtube-Client-Name`;
  omitting it would derive `"0"`, which matches no real client.)

### Profile refresh checklist

When a client starts returning playability `ERROR`, HTTP 400, empty formats, or
URLs that now need a PO token, refresh the profile deliberately:

- Verify `clientName`, numeric `innerTubeId`, version, user agent, device fields,
  and whether the client should post to a different InnerTube host.
- Recheck `requiresPoToken` against live behavior and current extractor
  references. If a client needs a player-scope token during extraction, wire that
  hook before moving it earlier in the default chain.
- Keep at least one playlist-capable profile (`WEB` today) in the chain unless
  playlist support has moved elsewhere.
- Add or update a fixture that captures the new behavior before changing parser
  or resolver code.

## On-disk cache

WaxTap persists YouTube's player JS (`base.js`, a few MiB that change only when
YouTube rotates the player) so a fresh process compiles the cipher from disk
instead of re-downloading it.

- Location: `waxtap cache dir` (default `os.UserCacheDir()/waxtap`, under
  `players/v<schema>/`). Override with `--cache-dir` / `WAXTAP_CACHE_DIR`.
- It is size-capped (LRU), schema-versioned, written `0600` with atomic
  temp+rename, and entirely best-effort: a read-only or full filesystem just
  degrades to network-only, it never fails extraction.
- Only genuine player JS is cached: a 200 response with no extractable cipher
  transform (a bot wall, captive portal, or truncated body) is never persisted,
  and an already-poisoned entry is ignored in favor of a fresh fetch. A
  transient bad response cannot wedge later runs.
- `waxtap cache clean` removes it; `--no-cache` / `WAXTAP_NO_CACHE` disables it.
- If you ever suspect a corrupt or stale player is cached, `cache clean` and
  re-run. WaxTap re-fetches whatever it needs.

## PO tokens

WaxTap does not ship a PO-token generator. It accepts a caller-supplied
`potoken.Provider` through `waxtap.Options.POTokenProvider`; if the winning
profile requires a token and no provider is configured, resolution fails with
`ErrNeedsPOToken`.

When integrating a provider:

- **Share the HTTP client and cookie jar.** Create a `*http.Client` with a jar and
  pass the same client to WaxTap (`Options.HTTPClient`) and the provider. That
  keeps the token minting request and the stream request on the same IP and
  browser/session identity. If you let WaxTap build its default client, its jar is
  internal to WaxTap and cannot be shared with an external provider.
- **Use the threaded user agent.** `potoken.Request.UserAgent` carries the exact
  UA the googlevideo request will send. Providers that bind tokens to request
  headers should mint with that UA.
- **Use the threaded failure.** On an expired or invalid stream token,
  `youtube.Client.ResolveWithFailure` passes the triggering
  `potoken.HTTPFailure` into the provider for diagnosis before re-resolving.

Only stream-time token requests are wired today. A player-scope PO hook at
extraction time should be added when a maintained profile actually requires it.

## Fixtures policy

- **Committed:** authored / minimized fixtures under `youtube/testdata/` and
  `youtube/internal/resolver/testdata/` (a hand-written `base.js` exercising the
  cipher locators, trimmed player-response JSON, etc.).
- **Never committed:** real YouTube `base.js` / player-response captures
  (licensing). `.gitignore` excludes `testdata/real/`, `*.real.js`, and
  `*.real.json`. Use real captures locally to author a minimal fixture, then
  delete them.

## Releasing

Release binaries (Linux/macOS/Windows, amd64/arm64) are built by GoReleaser.

```
git tag vX.Y.Z
git push origin vX.Y.Z      # triggers .github/workflows/release.yml
```

The workflow runs `goreleaser release --clean` and creates a **draft** GitHub
release for final review before publishing. For a local dry run, use
`goreleaser release --snapshot --clean`; for a config-only check, use
`goreleaser check`. After publishing,
`go install github.com/colespringer/waxtap/cmd/waxtap@vX.Y.Z` should also work
because the version subcommand reads module build info when no ldflags are
injected.
