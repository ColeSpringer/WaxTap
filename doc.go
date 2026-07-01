// Package waxtap provides the public WaxTap API for acquiring and processing
// audio.
//
// WaxTap can take audio from YouTube or from a local file. Processing stages
// such as transcoding, cutting, SponsorBlock removal, loudness measurement, and
// loudness normalization are opt-in. By default, downloads keep the selected
// source stream and do not re-encode it.
//
// [Client.Download] and [Client.Stream] handle one video.
// [Client.DownloadPlaylist] downloads playlist entries with bounded concurrency,
// optional pacing, and an optional limit on download attempts. [Options]
// configures per-host request rates and post-rate-limit cooldowns.
//
// The default client chain returns playable audio for public videos with no PO
// token. WEB-family clients need a [POTokenProvider] and remain experimental. For
// byte-exact session coherence with a token minter, [Options.Session] /
// [Options.SessionProvider] adopt an externally supplied guest visitorData and
// cookies verbatim instead of bootstrapping; adoption requires a uniform client
// chain ([Options.Client] or a single-family profile override) and resolves once
// per Client.
//
// This top-level package is the stable public surface. The youtube package and
// packages below it are YouTube-specific implementation surfaces; they are
// exported where the facade needs them, but external callers should prefer this
// package.
//
// # Type ownership
//
// To keep the dependency graph acyclic, contract types are defined in the
// package that owns the behavior and re-exported here for convenience:
//   - audio formats and selectors: package format
//   - the PO-token provider contract: package potoken
//   - the SponsorBlock category vocabulary: package sponsorblock
//   - the error taxonomy: package waxerr
//   - extraction models (Video, Playlist): package youtube
//
// Callers can work entirely through waxtap, using names such as
// [BestAudio], [ErrVideoUnavailable], and [POTokenProvider].
//
// # Identity anchors
//
// [Video.ID] (the 11-character video ID) and [Video.ChannelID] (the UC channel
// ID) are the canonical, stable YouTube identifiers. Callers that persist or
// deduplicate should key on these rather than on titles or URLs. [Video.URL] is
// the canonical watch URL derived from [Video.ID].
//
// # Availability errors: skip vs. fail
//
// Info and Download return typed availability sentinels for videos that exist but
// cannot be delivered. A consumer iterating a feed should treat these as "skip
// this item and continue," not as a hard failure, because retrying or updating
// the tool will not help:
//
//   - [ErrLiveContent]: the stream is currently live (retry after it ends)
//   - [ErrLiveNotStarted]: an upcoming premiere or offline stream (retry later)
//   - [ErrLoginRequired]: sign-in or an interactive confirm gate
//   - [ErrAgeRestricted]: age-gated (rare, since the default client bypasses age-gating)
//   - [ErrVideoRestricted]: private (maps a consumer's ErrPrivate)
//   - [ErrMembersOnly]: channel-members only
//   - [ErrGeoBlocked]: blocked in the request IP's region
//   - [ErrVideoUnavailable]: removed or generic-unavailable (maps a consumer's ErrRemoved)
//   - [ErrNoAudioFormats]: no audio rendition exists
//
// Everything else is a hard error the consumer should surface: extraction and
// cipher maintenance signals ([ErrExtractionFailed], [ErrCipherSolve],
// [ErrPlaylistParse]), rate limiting ([ErrRateLimited]), incomplete delivery
// ([ErrIncompleteStream], [ErrURLExpired]), token preconditions ([ErrNeedsPOToken]),
// and network or I/O failures. Match sentinels with errors.Is; a [PlayabilityError]
// (via errors.As / errors.AsType) carries YouTube's Status and Reason for finer
// classification. Members-only and geo-blocked matching is best-effort under the
// default en/US locale.
package waxtap
