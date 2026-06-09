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
package waxtap
