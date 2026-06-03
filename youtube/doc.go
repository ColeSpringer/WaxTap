// Package youtube performs YouTube extraction: it turns a URL into video
// metadata and candidate audio formats, and resolves those formats into
// playable, signed stream URLs.
//
// Stability: this package and everything beneath it, notably the internal
// resolver, are YouTube-specific implementation surfaces. They are exported for
// the facade where needed but may change between releases. External users should
// prefer the top-level waxtap package.
//
// Concurrency and identity: a [ClientProfile] is immutable, with headers cloned
// on read. Mutable per-attempt state lives in an unexported session, so
// concurrent extractions do not share writable client identity.
package youtube
