package waxtap

import (
	"encoding/json"
	"io"
	"math"
	"time"

	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/sponsorblock"
	"github.com/colespringer/waxtap/v3/youtube"
)

// Audio format model and selectors (package format).
type (
	Format           = format.Format
	AudioTrack       = format.AudioTrack
	Tri              = format.Tri
	AudioQualityTier = format.AudioQualityTier
	AudioSelector    = format.AudioSelector
	ChannelLayout    = format.ChannelLayout
	SourcePolicy     = format.SourcePolicy
	// Target describes a transcode output for source selection. The facade maps
	// a TranscodeSpec onto it; most callers do not construct one directly.
	Target = format.Target
)

// ErrNoMatch reports that audio selection found no candidate satisfying the
// request. Download/Process translate it to ErrNoAudioFormats; it is re-exported
// for callers using BestForTarget directly.
var ErrNoMatch = format.ErrNoMatch

// BestForTarget chooses the best source audio index for a transcode target under
// a SourcePolicy. It is the selection BestAudio uses; exposed for callers that
// resolve formats themselves.
func BestForTarget(candidates []Format, policy SourcePolicy, target Target) (int, error) {
	return format.BestForTarget(candidates, policy, target)
}

// Tri values.
const (
	Unknown = format.Unknown
	Yes     = format.Yes
	No      = format.No
)

// Audio quality tiers reported by YouTube.
const (
	QualityUnknown  = format.QualityUnknown
	QualityUltraLow = format.QualityUltraLow
	QualityLow      = format.QualityLow
	QualityMedium   = format.QualityMedium
	QualityHigh     = format.QualityHigh
)

// Channel layouts used by AudioSelector.WithChannels and ProcessSpec.Channels.
// LayoutAny is the neutral zero value.
const (
	LayoutAny      = format.LayoutAny
	LayoutMono     = format.LayoutMono
	LayoutStereo   = format.LayoutStereo
	LayoutSurround = format.LayoutSurround
)

// BestAudio selects the best audio stream. It prefers the original track,
// non-DRC audio, higher reported quality tiers, Opus within a tier, and finally
// higher effective bitrate.
//
// The selector itself imposes no channel preference, but the Download, Info, and
// Resolve facades apply a stereo default to it, so client.Download(Request{URL})
// yields stereo. Call WithChannels(LayoutSurround) for surround, or
// WithChannels(LayoutAny) to let a surround track rank highest.
func BestAudio() AudioSelector { return format.BestAudio() }

// Itag selects the stream with the exact itag.
func Itag(itag int) AudioSelector { return format.Itag(itag) }

// Codec selects the best stream whose codec matches (e.g. "opus", "aac").
func Codec(codec string) AudioSelector { return format.Codec(codec) }

// MinimizeLoss prefers a source in the target codec family, avoiding a
// cross-codec transcode when possible.
func MinimizeLoss() SourcePolicy { return format.MinimizeLoss() }

// BestNative ignores target codec matching and uses normal best-audio ranking.
func BestNative() SourcePolicy { return format.BestNative() }

// PreferCodec prefers a source in the named codec family when policy is active.
func PreferCodec(codec string) SourcePolicy { return format.PreferCodec(codec) }

// SponsorBlock types are re-exported so callers can configure [CutSpec] and call
// [Client.SponsorBlockSegments] without importing package sponsorblock.
type (
	// Category identifies a SponsorBlock segment category.
	Category = sponsorblock.Category
	// Segment describes one SponsorBlock skip segment.
	Segment = sponsorblock.Segment
)

// SponsorBlock categories. Values match the SponsorBlock API wire strings.
const (
	CategorySponsor       = sponsorblock.CategorySponsor
	CategorySelfPromo     = sponsorblock.CategorySelfPromo
	CategoryInteraction   = sponsorblock.CategoryInteraction
	CategoryIntro         = sponsorblock.CategoryIntro
	CategoryOutro         = sponsorblock.CategoryOutro
	CategoryPreview       = sponsorblock.CategoryPreview
	CategoryFiller        = sponsorblock.CategoryFiller
	CategoryMusicOffTopic = sponsorblock.CategoryMusicOffTopic
)

// DefaultCategories contains the categories used when [CutSpec.SponsorBlock] is
// a non-nil empty slice.
var DefaultCategories = sponsorblock.DefaultCategories

// Extraction models (package youtube). Part of the volatile surface; may evolve
// pre-1.0.
type (
	Video          = youtube.Video
	Thumbnail      = youtube.Thumbnail
	Chapter        = youtube.Chapter
	Playlist       = youtube.Playlist
	PlaylistEntry  = youtube.PlaylistEntry
	ResolvedStream = youtube.ResolvedStream
	// LiveStatus reports a video's live-broadcast state. On a Video from Info it is
	// LiveNone or LiveWasLive; live/upcoming videos surface as error sentinels.
	LiveStatus = youtube.LiveStatus
	// Availability reports whether a video is publicly listed. It is set only when a
	// watch-page metadata pass runs (see WithFullMetadata), else AvailabilityUnknown.
	Availability = youtube.Availability
)

// LiveStatus values.
const (
	LiveNone     = youtube.LiveNone
	LiveUpcoming = youtube.LiveUpcoming
	LiveNow      = youtube.LiveNow
	LiveWasLive  = youtube.LiveWasLive
)

// Availability values.
const (
	AvailabilityUnknown  = youtube.AvailabilityUnknown
	AvailabilityPublic   = youtube.AvailabilityPublic
	AvailabilityUnlisted = youtube.AvailabilityUnlisted
)

// PO-token provider contract (package potoken).
type (
	POTokenProvider = potoken.Provider
	POTokenRequest  = potoken.Request
	POTokenResponse = potoken.Response
	POTokenScope    = potoken.Scope
	// POTokenProviderFunc adapts a closure to POTokenProvider.
	POTokenProviderFunc = potoken.ProviderFunc
	// POTokenFailure describes the HTTP failure that triggered a token refresh.
	// [POTokenRequest].Failure points to a POTokenFailure when one is available.
	POTokenFailure = potoken.HTTPFailure
)

// PO-token scopes identify where a token will be used. Tokens are not
// interchangeable across scopes.
const (
	ScopeNone      = potoken.ScopeNone      // no token scope
	ScopePlayer    = potoken.ScopePlayer    // /player request body
	ScopeGVS       = potoken.ScopeGVS       // googlevideo media URL
	ScopeSubtitles = potoken.ScopeSubtitles // subtitle or timed-text URL
)

// External guest-session adoption (package potoken). A POTokenSession lets WaxTap
// adopt an externally supplied visitorData + cookies verbatim instead of
// bootstrapping its own, for byte-exact session coherence with a PO-token minter.
// POTokenSessionProvider is its pull-based form.
type (
	POTokenSession         = potoken.Session
	POTokenSessionProvider = potoken.SessionProvider
)

// Attested WEB player-context handoff (package potoken). A PlayerContextProvider
// supplies an attested /player streaming context (serverAbrStreamingUrl,
// ustreamer config, visitorData, and audio formats) that WaxTap streams Go-side,
// enabling the opt-in WEB SABR audio path.
type (
	PlayerContextProvider     = potoken.PlayerContextProvider
	PlayerContext             = potoken.PlayerContext
	PlayerContextFormat       = potoken.PlayerContextFormat
	PlayerContextProviderFunc = potoken.PlayerContextProviderFunc
)

// ProcessSpec is the processing pipeline shared by YouTube and local-file
// requests. Each stage is opt-in: a nil pointer means that stage is skipped, so
// the default path keeps the selected source stream unchanged.
type ProcessSpec struct {
	Transcode *TranscodeSpec // nil = keep source, no re-encode
	Cut       *CutSpec       // nil = no cut
	Loudness  *LoudnessSpec  // nil = no loudness work

	// Channels is the Downmix target layout, applied after probing. When Downmix is
	// set it must be LayoutMono or LayoutStereo; pairing Downmix with LayoutAny is a
	// hard error. LayoutAny, the zero value, means no downmix target, so Downmix must
	// be false. For YouTube requests, prefer setting the layout on Audio with
	// WithChannels to pick a native track; audio selection already defaults to stereo,
	// so downmix is only needed when a caller opts into a surround source.
	Channels ChannelLayout
	// Downmix reduces a source with more channels to Channels after probing. It
	// never adds channels and does nothing when the source already fits the
	// requested layout. Channels must be LayoutMono or LayoutStereo. When
	// Transcode is nil, the encoder is chosen from the source codec and destination
	// container.
	Downmix bool

	// Output is the sink. For source-style delivery (an io.ReadCloser to pipe
	// elsewhere) use Client.Stream instead of setting Output.
	Output Output

	// Events receives best-effort, synchronous, panic-recovered stage events.
	// It may be nil. A slow callback backpressures the worker, so keep it fast.
	Events func(Event)

	// SkipIfExists skips work when the exact output path already exists. This is
	// only a path check; callers remain responsible for library-level deduping.
	SkipIfExists bool

	// IncludeMetadata attaches extended video metadata to Result.Metadata for
	// YouTube downloads. It has no effect on local-file processing.
	IncludeMetadata bool

	// EmbedThumbnail embeds the YouTube thumbnail as front-cover art in the
	// written audio file, when the output format can carry a picture. It is
	// off by default and applies to YouTube downloads only, so it is a no-op on a
	// local-file [Client.Process] (which has no thumbnail to fetch). A fetch or
	// embed failure never fails the download.
	EmbedThumbnail bool

	// CoverArt selects the shape of the embedded cover picture. The zero value,
	// CoverArtFrame, stores the fetched bytes verbatim. It requires
	// EmbedThumbnail; setting it alone is ErrIncompatibleSpec rather than a
	// silently ignored field, and like EmbedThumbnail it is a no-op on a
	// local-file Process.
	CoverArt CoverArtMode

	// EmbedMetadata writes basic tags (title, artist, date, chapters) into the
	// written audio file, when the output format can carry them. It is off by
	// default and applies to YouTube downloads only. On the default token-free
	// path only title and artist are reliably available; date and chapters land
	// only when richer metadata is present (see FullMetadata). A failure never
	// fails the download.
	EmbedMetadata bool
}

// Request is a YouTube acquisition + processing request.
type Request struct {
	// URL is a YouTube video URL or bare video ID.
	URL string

	// Audio selects which audio stream to take. The zero value is BestAudio, which
	// the facade defaults to stereo; use BestAudio().WithChannels(LayoutSurround) for
	// surround or WithChannels(LayoutAny) to rank purely by fidelity.
	Audio AudioSelector
	// SourcePolicy controls source selection when transcoding. The zero value is
	// MinimizeLoss.
	SourcePolicy SourcePolicy

	// NoFallback prevents fallback from a WEB player context to the configured
	// client chain, disables watch-page extraction, and prevents retrying another
	// client after an incomplete download. The configured extraction chain may
	// still select a working client. Set Options.Client to force a single client.
	// Read methods use WithNoFallback for the same behavior.
	NoFallback bool

	// FullMetadata runs a token-free watch-page pass during the download, adding the
	// PublishDate and Chapters that the default /player client omits, so an ingest
	// that needs them is one call instead of a separate Info(..., WithFullMetadata())
	// plus Download. It requires IncludeMetadata or EmbedMetadata, the two consumers
	// of the enrichment, and feeds whichever is set: the fields reach Result.Metadata
	// only with IncludeMetadata (which is what populates it at all) and the written
	// tags only with EmbedMetadata. It is
	// a no-op with NoFallback (which forbids the watch page). The extra fetch is
	// skipped when extraction already scraped the watch page. Enrichment is
	// best-effort: a failure leaves the base metadata and never fails the download.
	FullMetadata bool

	ProcessSpec
}

// ProcessRequest processes a local audio file through the same pipeline as a
// YouTube download (transcode/cut/normalize), with no YouTube access.
type ProcessRequest struct {
	// Input is the local file path. Reader-based inputs will use a separate
	// request type; non-seekable inputs are staged before processing.
	Input string

	ProcessSpec
}

// TranscodeFormat names an output preset. FormatCopy is the only no-re-encode
// path. FLAC, ALAC, WAV, AIFF, WavPack, and APE preserve the decoded samples,
// but they are still decode-and-encode passes when the source is YouTube audio.
type TranscodeFormat uint8

const (
	FormatCopy   TranscodeFormat = iota // remux / stream-copy (no re-encode)
	FormatFLAC                          // FLAC lossless audio
	FormatALAC                          // Apple Lossless audio
	FormatWAV                           // uncompressed PCM in a WAV container
	FormatMP3                           // MP3 audio
	FormatAAC                           // delivered in an .m4a container
	FormatOpus                          // Opus audio
	FormatVorbis                        // Vorbis audio
	FormatAIFF                          // uncompressed PCM in an AIFF container
	// FormatHEAAC is HE-AAC v1 (SBR over a half-rate AAC-LC core), delivered in
	// an .m4a container like FormatAAC but aimed at low bitrates (the preset
	// default is 64 kbps against AAC-LC's 256). A request for FormatAAC on a
	// source that is already HE-AAC copies it under its own identity rather
	// than re-encoding to AAC-LC.
	FormatHEAAC
	// FormatWavPack is WavPack lossless audio (.wv). It holds mono or stereo
	// only; a wider source is refused rather than silently folded (set Downmix
	// to choose the fold). Metadata lands in an APEv2 block through the same
	// post-pass as every other format: text tags and cover art carry, while
	// chapters and synced lyrics have no APEv2 form and are reported as carry
	// losses.
	FormatWavPack
	// FormatAPE is Monkey's Audio lossless audio (.ape), with the same APEv2
	// metadata behavior as FormatWavPack. It holds 8/16/24-bit integer PCM in
	// mono or stereo only; a 32-bit integer source is refused rather than
	// silently narrowed (ask for BitDepth 24).
	FormatAPE
)

// TranscodeSpec requests re-encoding. An explicit FormatCopy remuxes (container
// copy, no re-encode) into the destination container; a nil TranscodeSpec
// keeps the selected source bytes untouched.
type TranscodeSpec struct {
	// Format selects the output preset.
	Format TranscodeFormat
	// Bitrate is the target bits per second for lossy presets (e.g. 256000).
	// Zero selects the preset default. Ignored by lossless presets.
	Bitrate int
	// BitDepth forces integer output at 16 or 24 bits for the presets that hold
	// integer PCM (WAV, AIFF, FLAC, ALAC, WavPack, APE). Zero, the default,
	// follows the decoded stream, so a lossy source (which decodes to float)
	// yields 32-bit float WAV and 24-bit FLAC. Narrowing is dithered, not
	// truncated. APE holds nothing wider than 24 bits, so a 32-bit integer
	// source needs BitDepth 24 there (see FormatAPE).
	//
	// The lossy presets encode in the float domain and ignore it silently, as does
	// FormatCopy; only the CLI notes that. A value outside {0, 16, 24} is still
	// rejected with ErrIncompatibleSpec even on a preset that would ignore it,
	// because it is a mistake about a value the caller believes will apply.
	BitDepth int
}

// CutMode selects how cuts are rendered.
type CutMode uint8

const (
	// CutSmart copies when cutting alone (lossless, frame-boundary) and fuses the
	// cut into the transcode when one is requested. It avoids cut-then-transcode
	// workflows that would encode twice.
	CutSmart CutMode = iota
	// CutCopy forces stream-copy; it errors with ErrIncompatibleSpec when copy is
	// unsafe for the codec/container, and also when the spec asks for an encode the
	// copy would have to give up: any TranscodeSpec.Format other than FormatCopy, or
	// a Downmix the source needs. Combining them is a contradiction, not a
	// preference to be silently resolved.
	CutCopy
	// CutAccurate decodes, cuts sample-exactly, and re-encodes.
	CutAccurate
)

// SponsorBlockErrorPolicy governs SponsorBlock fetch failures only (cut and
// transcode failures are always hard errors).
type SponsorBlockErrorPolicy uint8

const (
	// ProceedUncut logs a warning and delivers the full, uncut audio when the
	// SponsorBlock fetch fails or times out (the default).
	ProceedUncut SponsorBlockErrorPolicy = iota
	// FailDownload fails the whole request when the SponsorBlock fetch fails.
	FailDownload
)

// CutSpec describes time-range removal and/or SponsorBlock-driven cuts.
type CutSpec struct {
	// Ranges are explicit [Start, End) removals (optional). They are clamped to the
	// media duration. A request whose ranges all lie outside the media returns
	// ErrIncompatibleSpec; partial overlaps remain valid.
	Ranges []TimeRange
	// SponsorBlock lists categories to fetch and remove. Nil disables
	// SponsorBlock; a non-nil empty slice uses [DefaultCategories].
	SponsorBlock []Category
	// Mode selects copy/accurate/smart rendering.
	Mode CutMode
	// Crossfade, when > 0, applies a click-free crossfade at splice points. It
	// is OFF by default and orthogonal to Mode (accurate does not imply it).
	Crossfade time.Duration
	// OnError governs the SponsorBlock fetch only.
	OnError SponsorBlockErrorPolicy
	// Timeout is a strict cap on the SponsorBlock fetch.
	Timeout time.Duration
}

// LoudnessMode selects measurement vs. normalization.
type LoudnessMode uint8

const (
	// LoudnessMeasureOnly returns measurements without altering the audio.
	LoudnessMeasureOnly LoudnessMode = iota
	// LoudnessApply normalizes to Target, fused into the transcode pass. It
	// requires an encode, so it is rejected with FormatCopy or no transcode unless
	// an explicit output codec is given (ErrIncompatibleSpec).
	LoudnessApply
)

// PeakMode selects how normalization protects the true peak.
type PeakMode uint8

const (
	// PeakCap clamps the gain so the true peak stays under -1.0 dBTP. It is
	// transparent, but a source already peaking near 0 dBTP takes almost no gain
	// whatever the target, so the achieved loudness can land well short of it.
	PeakCap PeakMode = iota
	// PeakLimit applies the full gain and lets the true-peak limiter catch the
	// overshoot, reaching the target at the cost of transparency.
	//
	// One pass cannot get there: the limiter gives back part of whatever gain it is
	// handed, by an amount that depends on the material. So a PeakLimit run measures
	// its own output and corrects, re-encoding a small bounded number of times until
	// the delivered loudness lands within a fraction of a LU of the target, and
	// emitting [WarnLoudnessTargetMissed] when the limiter saturates before it can.
	// Ordinary material costs one extra encode; a source already crushed against the
	// ceiling costs a few and may still fall short, which is what the warning is for.
	//
	// Album normalization also limits, because a per-track clamp would destroy the
	// relative spacing album mode exists to preserve, but it stays a single pass for
	// that same reason: correcting each track individually would undo the spacing.
	PeakLimit
)

// CoverArtMode selects the shape of the embedded cover picture.
type CoverArtMode uint8

const (
	// CoverArtFrame embeds the fetched image verbatim, bars and all. It is the
	// zero value, so the delivered bytes stay byte-identical to what the CDN sent
	// and no re-encode generation is spent.
	CoverArtFrame CoverArtMode = iota
	// CoverArtSquare peels uniform-color borders and center-crops what remains to
	// a square, recovering the release art from the 16:9 canvas (and 4:3
	// letterbox) YouTube composites an Art Track onto. It crops and never scales,
	// so the art keeps its proportions, and an image that is already square within
	// 1% is left untouched: the result is square within 1%, not exactly square.
	CoverArtSquare
)

// LoudnessSpec requests loudness measurement or normalization (EBU R128).
type LoudnessSpec struct {
	// Mode selects measurement or applied normalization.
	Mode LoudnessMode
	// Target is the target integrated loudness in LUFS for Apply (e.g. -14). The
	// value is the caller's policy; WaxTap does not impose one.
	Target float64
	// PeakMode selects peak protection for Apply. The zero value, PeakCap, caps
	// the gain to hold the true peak under the ceiling.
	PeakMode PeakMode
}

// LoudnessInfo holds an EBU R128 measurement.
type LoudnessInfo struct {
	IntegratedLUFS float64 // integrated loudness, LUFS
	TruePeakDBTP   float64 // true peak, dBTP
	LRA            float64 // loudness range, LU
	SamplePeakDB   float64 // sample peak, dBFS
}

// MarshalJSON encodes non-finite measurements as JSON null because encoding/json
// rejects NaN and Inf. Silent tracks can produce -Inf; using null keeps
// LoudnessInfo JSON-friendly for Measure and MeasureAlbum callers. Field names
// stay the exported struct names.
func (l LoudnessInfo) MarshalJSON() ([]byte, error) {
	finite := func(v float64) *float64 {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil
		}
		return &v
	}
	return json.Marshal(struct {
		IntegratedLUFS *float64
		TruePeakDBTP   *float64
		LRA            *float64
		SamplePeakDB   *float64
	}{finite(l.IntegratedLUFS), finite(l.TruePeakDBTP), finite(l.LRA), finite(l.SamplePeakDB)})
}

// LoudnessResult reports loudness measurements. WaxTap returns LUFS/true-peak
// measurements, not ReplayGain tag values.
type LoudnessResult struct {
	Input  *LoudnessInfo // measured input loudness (post-cut)
	Output *LoudnessInfo // post-apply loudness; set only when Mode == LoudnessApply
	Target float64       // requested integrated loudness in LUFS
	// GainDB is the gain normalization applied, in dB: the encode's scalar, or
	// under HeaderGain the amount the Opus header's output gain was moved by,
	// quantized to its Q7.8 step. It is a change, not a total: a source whose
	// head already stated a gain keeps it, and the head the file leaves with
	// states the sum. 0 unless LoudnessApplied.
	GainDB float64
	// HeaderGain says the gain rode in the Opus header (OpusHead output gain)
	// with the packets copied untouched, rather than being applied to the
	// samples by a re-encode. Every compliant decoder applies it, so Output
	// reads the normalized loudness and Transcoded stays false. A run whose
	// gain quantized to nothing leaves it false: the head did not move, and
	// the file is a plain copy.
	HeaderGain bool
}

// TimeRange is a half-open [Start, End) span. End must be greater than Start.
type TimeRange struct {
	Start time.Duration // inclusive start offset
	End   time.Duration // exclusive end offset
}

type outputKind uint8

const (
	outputNone outputKind = iota
	outputFile
	outputWriter
)

// Output is a delivery sink: either a file path or a writer. The zero value is
// unset. Construct it with ToFile or ToWriter.
//
// The library writes the exact path given to ToFile (only a temp suffix and an
// atomic rename); filename templating, sanitization, and collision handling are
// the CLI's job, not the library's.
type Output struct {
	kind      outputKind
	path      string
	writer    io.Writer
	exclusive bool
	renumber  bool
}

// ToFile delivers to an exact file path (written atomically via a temp + rename),
// replacing an existing file.
func ToFile(path string) Output { return Output{kind: outputFile, path: path} }

// ToNewFile delivers to a file path that must not already exist, failing with
// fs.ErrExist when one is.
//
// The publish itself is exclusive rather than a check before it, so two runs
// racing for one path cannot both report success: the loser fails and the
// winner's file is intact. Prefer it over ToFile plus a stat whenever an
// existing file is an error. A filesystem without hard links degrades to a stat
// and a rename, where a concurrent writer can still be lost.
func ToNewFile(path string) Output {
	return Output{kind: outputFile, path: path, exclusive: true}
}

// ToNewNumberedFile delivers to path or, when the publish finds it taken, to the
// first free "name (n)" sibling, claimed with the same exclusive publish
// ToNewFile uses. The delivered path is Result.OutputPath.
//
// It exists for auto-number collision policies, whose pre-flight pick is a stat
// and so cannot see a writer that claims the name in between. N runs racing for
// one basename therefore produce N files. The numbering is not deterministic
// under concurrency: racing runs can land (2) and (3) with (1) belonging to
// neither.
func ToNewNumberedFile(path string) Output {
	return Output{kind: outputFile, path: path, exclusive: true, renumber: true}
}

// ToWriter delivers to a caller-provided writer (bounded memory, no atomicity
// guarantee).
func ToWriter(w io.Writer) Output { return Output{kind: outputWriter, writer: w} }

// SourceKind distinguishes a YouTube download from local-file processing.
type SourceKind uint8

const (
	SourceYouTube   SourceKind = iota // media acquired from YouTube
	SourceLocalFile                   // media read from a local file
)

func (k SourceKind) String() string {
	switch k {
	case SourceLocalFile:
		return "local-file"
	default:
		return "youtube"
	}
}

// Result reports the outcome of a Download or Process. Boolean flags describe
// completed effects, not requested work. For example, a SponsorBlock request
// that matches no segments leaves SponsorBlockApplied and CutApplied false.
type Result struct {
	SourceKind SourceKind // identifies a YouTube or local-file source
	VideoID    string     // empty for local files
	Title      string     // empty for local files
	InputPath  string     // set for local files
	OutputPath string     // empty for ToWriter delivery
	Client     string     // YouTube client used, such as "ANDROID_VR"; empty for local files
	// ViaWatchPage reports that the delivery came from the watch-page scrape
	// rather than the player endpoint. The client name is the same either way,
	// so this is the only thing that separates the two, and the two differ in
	// what metadata they carry and what they need to work.
	ViaWatchPage bool

	SourceFormat Format // input/source format
	OutputFormat Format // after transcode (== source when copy/keep)

	SourceBytes int64 // bytes read from the acquired or local source
	OutputBytes int64 // bytes delivered to the output sink

	Transcoded          bool            // audio was re-encoded (not stream-copied); a copy/remux stays false
	CutApplied          bool            // at least one time range was removed
	SponsorBlockApplied bool            // SponsorBlock contributed a removed range
	LoudnessMeasured    bool            // measured != normalized
	LoudnessApplied     bool            // normalization was applied
	Loudness            *LoudnessResult // nil unless measured

	Warnings []Warning // non-fatal conditions encountered during processing

	// TagCarry itemizes what a local process did with the input's embedded
	// metadata (tags, cover art, chapters, synced lyrics), the facts the
	// WarnTagCarry warning tells in prose. It is nil when no carry ran; see
	// TagCarry.
	TagCarry *TagCarry

	// Metadata contains extended video metadata when ProcessSpec.IncludeMetadata
	// is set. It is nil otherwise.
	Metadata *VideoMetadata
}

// VideoMetadata contains optional YouTube metadata that is not stored directly
// on Result.
type VideoMetadata struct {
	Author      string        // channel / uploader name
	ChannelID   string        // YouTube channel ID (canonical UC identity anchor)
	Duration    time.Duration // video duration, 0 if unknown
	PublishDate time.Time     // publication date, zero if unknown
	Description string        // video description
	// Availability is the listing state. It is Public or Unlisted only when
	// Request.FullMetadata ran the watch-page pass that determines it; otherwise
	// AvailabilityUnknown.
	Availability Availability
	// Chapters are the video's chapter markers. They are populated only when
	// Request.FullMetadata ran the watch-page pass (the default /player response
	// omits them); nil otherwise.
	Chapters []Chapter
	Formats  []Format // full candidate audio (and incidental video) formats
}

// StreamInfo is the initial metadata returned by Client.Stream alongside the
// stream reader. Final byte counts are known only after read-to-EOF/Close.
type StreamInfo struct {
	VideoID       string // resolved YouTube video ID
	Title         string // extracted video title
	Format        Format // selected source format
	ContentLength int64  // 0 if unknown
	Client        string // YouTube client used, such as "ANDROID_VR"
}

// EnumerateOptions tunes playlist enumeration. Enumeration never downloads.
type EnumerateOptions struct {
	// MaxItems caps the number of entries returned (0 = all).
	MaxItems int
	// Enrich refreshes entries with InfoBasic calls made at bounded concurrency.
	// A successful call overlays its entry's title, author, and duration with
	// what it fetched, keeping a listing value where the fetch had none, and
	// attaches the fetch as PlaylistEntry.Video, whose doc states the rule; a
	// failed one is added to Playlist.Errors as an EnrichError naming the entry.
	//
	// This is also where the metadata throttle is escaped: a session that has
	// asked about enough videos is refused the rest, worded exactly like a
	// removed video, and Enumerate retires its guest identity and re-asks to
	// tell the two apart. A lone Info call cannot, so a caller with a per-entry
	// budget should spend it here, through MaxEnrich, rather than on Info calls
	// of its own. The escape needs an identity Enumerate can retire: the
	// default client's cookie jar, or a SessionProvider that implements
	// potoken.SessionInvalidator. A jarless HTTPClient, a static Session, or a
	// provider without one gets no rotation, and a throttled entry is then
	// reported as it came, ErrVideoUnavailable.
	//
	// Entries the listing marked live or upcoming are passed over: Info
	// refuses them, so the call would spend a request to learn what
	// PlaylistEntry.LiveStatus already says. They keep a nil Video and are
	// not in Playlist.Errors.
	Enrich bool
	// MaxEnrich caps how many entries Enrich refreshes: the first n fetchable
	// entries returned, after Skip, Stop, and MaxItems (0 = every entry; live
	// and upcoming ones are passed over and do not count). Entries past the
	// cap are returned as listed, with a nil Video. It requires Enrich.
	MaxEnrich int
	// EnrichOptions apply to each enrichment's Info call, as the same options
	// do on Info at InfoBasic: WithFullMetadata adds the watch-page pass
	// (publish date, chapters, availability) at one more fetch per entry, and
	// WithNoFallback forbids that fetch along with the watch-page extraction
	// fallback. Selection options are inert at InfoBasic. The pass is
	// best-effort, as on Info; Video.Availability stays AvailabilityUnknown
	// when it did not run, which tells a video without chapters or a date from
	// a fetch that failed. It requires Enrich.
	EnrichOptions []ReadOption

	// Skip omits entries whose video ID it matches while continuing to page, for an
	// archive cursor. The consumer owns persistence: pass a predicate that reads
	// your store. It is safe on any playlist (seen items may be interspersed) and
	// runs before MaxItems, so the cap counts unseen entries. Skipped entries still
	// advance PlaylistEntry.Index, which stays the true playlist position.
	Skip func(id string) bool
	// Stop halts pagination at the first entry it matches (excluding that entry and
	// everything after) and leaves Playlist.Continuation empty. It is only correct
	// on an append-only newest-first feed such as a channel uploads playlist, where
	// a subscription poll stops at the first already-seen ID instead of paging the
	// whole feed. On a curated playlist, which can insert entries anywhere, Stop
	// drops items; use Skip there. Stop is checked before Skip.
	Stop func(id string) bool

	// OnProgress reports the running entry count after each playlist page. It is
	// optional and never triggers downloads.
	OnProgress func(items int)
	// OnEnrichProgress reports each completed InfoBasic refresh when Enrich is set.
	// Calls are serialized in increasing done-count order. total counts the
	// entries enrichment attempts, which MaxEnrich, and the live and upcoming
	// entries it passes over, can hold below len(Entries).
	// The final call reaches (total, total) unless context cancellation stops
	// enrichment early. Without Enrich it is never called.
	OnEnrichProgress func(done, total int)
}

// Stage identifies a pipeline stage in an Event.
type Stage uint8

const (
	StageExtracting  Stage = iota // fetching and parsing source metadata
	StageResolving                // resolving the selected media stream
	StageDownloading              // transferring source bytes
	StageStaging                  // preparing a local working file
	StageProbing                  // inspecting media
	StageAnalyzing                // measuring loudness
	StageCutting                  // removing time ranges
	StageNormalizing              // applying loudness normalization
	StageTranscoding              // encoding audio
	StageFinalizing               // delivering the completed output
	StageSkipped                  // skipping work because output already exists
	StageWarning                  // reporting a non-fatal warning
	StageDone                     // reporting successful completion
	StageFailed                   // reporting terminal failure
	// StageRemuxing copies packets into another container with no re-encode. It is
	// appended rather than placed beside StageTranscoding so the existing values
	// keep their numbers.
	StageRemuxing
)

func (s Stage) String() string {
	switch s {
	case StageExtracting:
		return "extracting"
	case StageResolving:
		return "resolving"
	case StageDownloading:
		return "downloading"
	case StageStaging:
		return "staging"
	case StageProbing:
		return "probing"
	case StageAnalyzing:
		return "analyzing"
	case StageCutting:
		return "cutting"
	case StageNormalizing:
		return "normalizing"
	case StageTranscoding:
		return "transcoding"
	case StageRemuxing:
		return "remuxing"
	case StageFinalizing:
		return "finalizing"
	case StageSkipped:
		return "skipped"
	case StageWarning:
		return "warning"
	case StageDone:
		return "done"
	case StageFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// WarningCode is a stable, machine-readable warning identifier. Warning.Detail
// is intended for people.
type WarningCode uint8

const (
	WarnProceedUncut        WarningCode = iota // SponsorBlock fetch failed; delivered uncut
	WarnFallbackProfile                        // a fallback client profile was used
	WarnURLReResolved                          // an expired stream URL was re-resolved
	WarnPlaylistEntryFailed                    // one playlist entry failed (others returned)
	WarnRateLimitedRetried                     // a request was retried after a 429
	WarnSponsorBlockEmpty                      // SponsorBlock matched no segments
	WarnRangesEmpty                            // SponsorBlock segments all fell outside the media
	WarnThrottled                              // a limiter/cooldown is active
	WarnWebContextFallback                     // WEB player-context failed; fell back to the configured chain
	WarnIncompleteFallback                     // a client returned an incomplete stream; switched clients
	WarnWebContextRetry                        // WEB player-context was capped (status 2); retried once with a fresh context
	// WarnMetadataEmbed reports that an --embed-thumbnail/--embed-metadata
	// post-pass could not write everything asked for. That includes partial
	// success: tags written but the picture dropped, or the picture embedded but
	// not shaped as --cover-art asked. Its String stays "metadata-embed-failed",
	// the identifier callers already match on; Detail says what was missed.
	WarnMetadataEmbed
	WarnLoudnessTargetMissed // peak capping held the gain back, so the loudness target was not reached
	WarnSessionRotated       // a fresh session replaced one whose stream URLs the server kept rejecting
	// WarnTagCarry reports that a local process could not carry all of the
	// input's embedded metadata onto the output: an item was dropped or
	// downgraded by the output format, chapters were dropped because a cut
	// changed the timeline, or the carry write failed. Detail names what was
	// lost.
	WarnTagCarry
	// WarnImplicitDownmix reports that the encoder folded channels the request
	// never asked to lose, because the output format cannot hold the source
	// layout. It is the only signal such a run has: nothing in the request says
	// "6 channels", so a consumer has nothing to compare the output against.
	WarnImplicitDownmix
	// WarnOutputClipping reports that the delivered file's level is past full
	// scale: samples the integer output could not carry were clamped, or the
	// waveform between stored samples crosses full scale and playback can clip
	// it. Detail carries WaxFlow's measurement of the delivered encode.
	WarnOutputClipping
	// WarnImplicitLossy reports that a lossless source was re-encoded to a
	// lossy codec the request never named: the spec asked for a copy (or
	// nothing), and automatic processing picked the output container's default
	// encoder because the source codec cannot enter that container. The result
	// line already names the codec written; this is the signal that quality was
	// lost where none of the request said it would be.
	WarnImplicitLossy
	// WarnInputDamage reports problems in a local input the decoder worked
	// around: a header declaring more audio than the file holds, bytes that did
	// not parse, or a decode that ended short of the declared length. Detail
	// leads with the verdict and carries the decoder's notes in its own words
	// (or the short-decode observation); the engine's remarks on a file that
	// plays fine are [WarnInputNote]'s, never this one's. The run still
	// succeeds, because the readable audio is real audio and refusing it would
	// help nobody.
	//
	// The list is complete once the output is written, not once the input is
	// probed: a demuxer that walks its payload lazily (MP3, bare or inside a
	// WAV or AIFF-C; ADTS) finds damage past the head only when the read
	// reaches it. Absence is still not a clean bill of health. Damage that
	// leaves a file the right length and its headers consistent (a rewritten
	// frame in the middle) probes without complaint; only a decode reaching it
	// surfaces the short-decode note.
	WarnInputDamage
	// WarnLoudnessUnmeasurable reports an integrated loudness that came back
	// non-finite, and why. Detail names the side ("input" or "output") and the
	// cause: too short to gate, digital silence, or a signal the R128 gates
	// removed entirely. The measurement itself is still reported as null, which
	// is honest but says nothing; this says what happened.
	WarnLoudnessUnmeasurable
	// WarnSourcePolicyUnmatched reports that --source-policy prefer:<codec>
	// named a codec family this video does not offer, so the preference had no
	// effect and selection ran as if none had been given. Detail names the
	// preference, the families that were available, and what was delivered.
	// A preference that was available but outranked by a better source stays
	// silent: that is the soft bias working as documented.
	WarnSourcePolicyUnmatched
	// WarnEmptyInput reports a local input whose audio track holds no frames: a
	// container that parses and declares a codec, carrying nothing. The run
	// still succeeds, and the output is a valid file of no audio, because that
	// is a faithful conversion of what was handed in and a batch that stops on
	// one empty file helps nobody.
	//
	// It is distinct from [WarnInputDamage], which is about audio that partly
	// read, and from [WarnLoudnessUnmeasurable], which explains a measurement
	// this condition also causes. A cut is the one request that refuses instead,
	// since there is nothing to cut.
	WarnEmptyInput
	// WarnInputNote relays what the engine did with a local input that is not
	// damaged, in its own words: a stream it ignored, a chapter list it capped
	// at its own limit, a timeline it rescaled, a band its decoder does not
	// synthesize, a delay field it declined to apply. None of it is damage,
	// which is why it is not [WarnInputDamage]: the file is well formed and
	// plays, and a listener who wants to know why the output is not quite the
	// input's shape reads it here. Only local processing raises it.
	WarnInputNote
)

func (w WarningCode) String() string {
	switch w {
	case WarnProceedUncut:
		return "proceed-uncut"
	case WarnFallbackProfile:
		return "fallback-profile"
	case WarnURLReResolved:
		return "url-re-resolved"
	case WarnPlaylistEntryFailed:
		return "playlist-entry-failed"
	case WarnRateLimitedRetried:
		return "rate-limited-retried"
	case WarnSponsorBlockEmpty:
		return "sponsorblock-empty"
	case WarnRangesEmpty:
		return "ranges-empty"
	case WarnThrottled:
		return "throttled"
	case WarnWebContextFallback:
		return "web-context-fallback"
	case WarnIncompleteFallback:
		return "incomplete-fallback"
	case WarnWebContextRetry:
		return "web-context-retry"
	case WarnMetadataEmbed:
		return "metadata-embed-failed"
	case WarnLoudnessTargetMissed:
		return "loudness-target-missed"
	case WarnSessionRotated:
		return "session-rotated"
	case WarnTagCarry:
		return "tag-carry-incomplete"
	case WarnImplicitDownmix:
		return "implicit-downmix"
	case WarnOutputClipping:
		return "output-clipping"
	case WarnImplicitLossy:
		return "implicit-lossy"
	case WarnInputDamage:
		return "input-damage"
	case WarnLoudnessUnmeasurable:
		return "loudness-unmeasurable"
	case WarnSourcePolicyUnmatched:
		return "source-policy-unmatched"
	case WarnEmptyInput:
		return "empty-input"
	case WarnInputNote:
		return "input-note"
	default:
		return "unknown"
	}
}

// Warning is a typed, non-fatal signal. It is both delivered as a StageWarning
// Event and accumulated in Result.Warnings.
type Warning struct {
	Code   WarningCode // stable machine-readable identifier
	Detail string      // human-readable context
}

// Event is a best-effort progress signal. Callbacks are invoked synchronously
// from the worker and are panic-recovered. A terminal event always fires:
// StageDone on success or StageFailed with Err. For Stream, the terminal event
// is emitted when the returned reader is closed.
type Event struct {
	Stage   Stage  // current pipeline stage
	VideoID string // empty for local-file processing

	// Downloading progress.
	Bytes int64
	Total int64 // 0 if unknown

	// CLI playlist expansion.
	ItemIndex int
	ItemCount int // total playlist entries, or 0 when unknown

	Warning *Warning // set when Stage == StageWarning
	Err     error    // set when Stage == StageFailed
	Message string   // optional human-readable detail
}
