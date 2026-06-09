package sabr

import "google.golang.org/protobuf/encoding/protowire"

// SABR uses a small set of protobuf messages. They are encoded and decoded
// directly with protowire to avoid generated code. Decoders ignore unknown
// fields so additions remain compatible.
//
// YouTube can change these field numbers. They were checked against
// upstreamCommit; if SABR decoding breaks after a protocol change, recheck the
// definitions before changing parser logic.

// upstreamCommit records the LuanRT/googlevideo revision used to verify the
// field numbers below. See protos/video_streaming/*.proto,
// protos/misc/common.proto, and protos/video_streaming/ump_part_id.proto.
const upstreamCommit = "d2fa40d761034a286cf60ee033653307a1295b0c" // LuanRT/googlevideo, 2025-11-03

// enabledTrackTypesAudioOnly is EnabledTrackTypes.AUDIO_ONLY. Format IDs select
// the stream; this bitfield limits requests to audio tracks.
const enabledTrackTypesAudioOnly int32 = 1

// Field numbers. Grouped per message; see upstreamCommit for provenance.
const (
	// misc.FormatId
	fFormatItag         protowire.Number = 1
	fFormatLastModified protowire.Number = 2
	fFormatXTags        protowire.Number = 3

	// misc.Range
	fRangeStart protowire.Number = 3
	fRangeEnd   protowire.Number = 4

	// BufferedRange
	fBufRangeFormatID     protowire.Number = 1
	fBufRangeStartTimeMs  protowire.Number = 2
	fBufRangeDurationMs   protowire.Number = 3
	fBufRangeStartSegment protowire.Number = 4
	fBufRangeEndSegment   protowire.Number = 5

	// ClientAbrState
	fAbrStatePlayerTimeMs  protowire.Number = 28
	fAbrStateEnabledTracks protowire.Number = 40

	// ClientInfo (nested in StreamerContext)
	fClientInfoDeviceMake     protowire.Number = 12
	fClientInfoDeviceModel    protowire.Number = 13
	fClientInfoClientName     protowire.Number = 16
	fClientInfoClientVersion  protowire.Number = 17
	fClientInfoOSName         protowire.Number = 18
	fClientInfoOSVersion      protowire.Number = 19
	fClientInfoAcceptLanguage protowire.Number = 21

	// StreamerContext
	fStreamerCtxClientInfo         protowire.Number = 1
	fStreamerCtxPOToken            protowire.Number = 2
	fStreamerCtxPlaybackCookie     protowire.Number = 3
	fStreamerCtxSabrContexts       protowire.Number = 5
	fStreamerCtxUnsentSabrContexts protowire.Number = 6

	// StreamerContext.SabrContext (nested in sabr_contexts)
	fSabrContextType  protowire.Number = 1
	fSabrContextValue protowire.Number = 2

	// VideoPlaybackAbrRequest
	fAbrClientState protowire.Number = 1
	// fAbrSelectedAudioFormats is selected_audio_format_ids (16), the form the
	// browser/yt-dlp/googlevideo use. The older selected_format_ids (2) does not
	// prompt the server to send the WebM init segment, so WEB SABR stalled.
	fAbrSelectedAudioFormats protowire.Number = 16
	fAbrBufferedRanges       protowire.Number = 3
	fAbrPlayerTimeMs         protowire.Number = 4
	fAbrUstreamerConfig      protowire.Number = 5
	fAbrStreamerContext      protowire.Number = 19

	// MediaHeader (UMP part 20)
	fMediaHdrHeaderID      protowire.Number = 1
	fMediaHdrItag          protowire.Number = 3
	fMediaHdrLastModified  protowire.Number = 4
	fMediaHdrXTags         protowire.Number = 5
	fMediaHdrIsInitSeg     protowire.Number = 8
	fMediaHdrSequenceNum   protowire.Number = 9
	fMediaHdrStartMs       protowire.Number = 11
	fMediaHdrDurationMs    protowire.Number = 12
	fMediaHdrFormatID      protowire.Number = 13
	fMediaHdrContentLength protowire.Number = 14

	// FormatInitializationMetadata (UMP part 42)
	fFmtInitFormatID      protowire.Number = 2
	fFmtInitEndSegment    protowire.Number = 4
	fFmtInitMimeType      protowire.Number = 5
	fFmtInitInitRange     protowire.Number = 6
	fFmtInitIndexRange    protowire.Number = 7
	fFmtInitDurationUnits protowire.Number = 9
	fFmtInitDurationScale protowire.Number = 10

	// NextRequestPolicy (UMP part 35)
	fNextPolicyReadaheadMs protowire.Number = 1
	fNextPolicyBackoffMs   protowire.Number = 4
	fNextPolicyCookie      protowire.Number = 7

	// SabrRedirect (UMP part 43)
	fSabrRedirectURL protowire.Number = 1

	// SabrError (UMP part 44)
	fSabrErrorType protowire.Number = 1
	fSabrErrorCode protowire.Number = 2

	// StreamProtectionStatus (UMP part 58)
	fProtStatusStatus     protowire.Number = 1
	fProtStatusMaxRetries protowire.Number = 2

	// SabrContextUpdate (UMP part 57)
	fSabrCtxUpdateType          protowire.Number = 1
	fSabrCtxUpdateScope         protowire.Number = 2
	fSabrCtxUpdateValue         protowire.Number = 3
	fSabrCtxUpdateSendByDefault protowire.Number = 4
	fSabrCtxUpdateWritePolicy   protowire.Number = 5

	// SabrContextSendingPolicy (UMP part 59)
	fSabrSendPolStart   protowire.Number = 1
	fSabrSendPolStop    protowire.Number = 2
	fSabrSendPolDiscard protowire.Number = 3
)

// SabrContextUpdate.write_policy values. A KEEP_EXISTING update must not
// overwrite a value already stored for its type. (0=UNSPECIFIED and 1=OVERWRITE
// both store the new value, so only KEEP_EXISTING needs a named constant.)
const writePolicyKeepExisting int32 = 2

// FormatId identifies a specific encoding; re-encodes can share an itag. It
// maps to misc.FormatId.
type FormatId struct {
	Itag         int32
	LastModified uint64
	XTags        string
}

func (f FormatId) marshal() []byte {
	var b []byte
	b = appendVarint(b, fFormatItag, uint64(f.Itag))
	if f.LastModified != 0 {
		b = appendVarint(b, fFormatLastModified, f.LastModified)
	}
	if f.XTags != "" {
		b = appendString(b, fFormatXTags, f.XTags)
	}
	return b
}

func unmarshalFormatId(b []byte) (FormatId, error) {
	var f FormatId
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fFormatItag && typ == protowire.VarintType:
			f.Itag = int32(r.varint())
		case num == fFormatLastModified && typ == protowire.VarintType:
			f.LastModified = r.varint()
		case num == fFormatXTags && typ == protowire.BytesType:
			f.XTags = r.string()
		default:
			r.skip(num, typ)
		}
	}
	return f, r.err
}

// ByteRange maps to misc.Range (the start/end pair, fields 3/4).
type ByteRange struct {
	Start int64
	End   int64
}

func unmarshalByteRange(b []byte) (ByteRange, error) {
	var rng ByteRange
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fRangeStart && typ == protowire.VarintType:
			rng.Start = int64(r.varint())
		case num == fRangeEnd && typ == protowire.VarintType:
			rng.End = int64(r.varint())
		default:
			r.skip(num, typ)
		}
	}
	return rng, r.err
}

// BufferedRange tells the server which segments the client already holds, so the
// next request returns the segments that follow.
type BufferedRange struct {
	FormatId          FormatId
	StartTimeMs       int64
	DurationMs        int64
	StartSegmentIndex int32
	EndSegmentIndex   int32
}

func (br BufferedRange) marshal() []byte {
	var b []byte
	b = appendBytes(b, fBufRangeFormatID, br.FormatId.marshal())
	b = appendVarint(b, fBufRangeStartTimeMs, uint64(br.StartTimeMs))
	b = appendVarint(b, fBufRangeDurationMs, uint64(br.DurationMs))
	b = appendVarint(b, fBufRangeStartSegment, uint64(br.StartSegmentIndex))
	b = appendVarint(b, fBufRangeEndSegment, uint64(br.EndSegmentIndex))
	return b
}

// clientAbrState carries playback position and the enabled-track bitfield.
type clientAbrState struct {
	PlayerTimeMs      int64
	EnabledTrackTypes int32
}

func (s clientAbrState) marshal() []byte {
	var b []byte
	b = appendVarint(b, fAbrStatePlayerTimeMs, uint64(s.PlayerTimeMs))
	b = appendVarint(b, fAbrStateEnabledTracks, uint64(s.EnabledTrackTypes))
	return b
}

// ClientInfo is the wire identity inside StreamerContext. ClientName is the
// numeric InnerTube client id (e.g. 1 for WEB), not the string name.
type ClientInfo struct {
	ClientName     int32
	ClientVersion  string
	OSName         string
	OSVersion      string
	DeviceMake     string
	DeviceModel    string
	AcceptLanguage string
}

func (c ClientInfo) marshal() []byte {
	var b []byte
	if c.DeviceMake != "" {
		b = appendString(b, fClientInfoDeviceMake, c.DeviceMake)
	}
	if c.DeviceModel != "" {
		b = appendString(b, fClientInfoDeviceModel, c.DeviceModel)
	}
	if c.ClientName != 0 {
		b = appendVarint(b, fClientInfoClientName, uint64(c.ClientName))
	}
	if c.ClientVersion != "" {
		b = appendString(b, fClientInfoClientVersion, c.ClientVersion)
	}
	if c.OSName != "" {
		b = appendString(b, fClientInfoOSName, c.OSName)
	}
	if c.OSVersion != "" {
		b = appendString(b, fClientInfoOSVersion, c.OSVersion)
	}
	if c.AcceptLanguage != "" {
		b = appendString(b, fClientInfoAcceptLanguage, c.AcceptLanguage)
	}
	return b
}

// streamerContext carries the client identity, GVS PO token, the playback
// cookie returned by the previous response, and any SABR contexts the server
// asked the client to echo back (see SabrContextUpdate).
type streamerContext struct {
	ClientInfo     ClientInfo
	POToken        []byte
	PlaybackCookie []byte
	// SabrContexts are the active contexts echoed to the server (field 5).
	SabrContexts []SabrContext
	// UnsentSabrContexts are the types the client holds but is not currently
	// sending (field 6).
	UnsentSabrContexts []int32
}

func (s streamerContext) marshal() []byte {
	var b []byte
	b = appendBytes(b, fStreamerCtxClientInfo, s.ClientInfo.marshal())
	if len(s.POToken) > 0 {
		b = appendBytes(b, fStreamerCtxPOToken, s.POToken)
	}
	if len(s.PlaybackCookie) > 0 {
		b = appendBytes(b, fStreamerCtxPlaybackCookie, s.PlaybackCookie)
	}
	// Fields 5 and 6 follow playback_cookie in field-number order. Per the proto2
	// reference client, unsent_sabr_contexts is emitted unpacked (one varint per
	// value); the server accepts both packed and unpacked forms.
	for _, c := range s.SabrContexts {
		b = appendBytes(b, fStreamerCtxSabrContexts, c.marshal())
	}
	for _, t := range s.UnsentSabrContexts {
		b = appendVarint(b, fStreamerCtxUnsentSabrContexts, uint64(t))
	}
	return b
}

// SabrContext is one (type, value) pair echoed back to the server in
// streamerContext.sabr_contexts. The value is the blob a SabrContextUpdate
// delivered for that type.
type SabrContext struct {
	Type  int32
	Value []byte
}

func (c SabrContext) marshal() []byte {
	var b []byte
	b = appendVarint(b, fSabrContextType, uint64(c.Type))
	if len(c.Value) > 0 {
		b = appendBytes(b, fSabrContextValue, c.Value)
	}
	return b
}

// videoPlaybackAbrRequest is the protobuf body POSTed to serverAbrStreamingUrl.
type videoPlaybackAbrRequest struct {
	ClientAbrState         clientAbrState
	SelectedAudioFormatIds []FormatId
	BufferedRanges         []BufferedRange
	PlayerTimeMs           int64
	UstreamerConfig        []byte
	StreamerContext        streamerContext
}

func (req videoPlaybackAbrRequest) marshal() []byte {
	var b []byte
	b = appendBytes(b, fAbrClientState, req.ClientAbrState.marshal())
	for _, f := range req.SelectedAudioFormatIds {
		b = appendBytes(b, fAbrSelectedAudioFormats, f.marshal())
	}
	for _, br := range req.BufferedRanges {
		b = appendBytes(b, fAbrBufferedRanges, br.marshal())
	}
	if req.PlayerTimeMs != 0 {
		b = appendVarint(b, fAbrPlayerTimeMs, uint64(req.PlayerTimeMs))
	}
	if len(req.UstreamerConfig) > 0 {
		b = appendBytes(b, fAbrUstreamerConfig, req.UstreamerConfig)
	}
	b = appendBytes(b, fAbrStreamerContext, req.StreamerContext.marshal())
	return b
}

// MediaHeader (UMP part 20) describes one media or init segment. HeaderID links
// it to the MEDIA parts that carry its bytes.
type MediaHeader struct {
	HeaderID       uint32
	Itag           int32
	LastModified   uint64
	XTags          string
	IsInitSeg      bool
	SequenceNumber uint64
	StartMs        int64
	DurationMs     int64
	FormatId       FormatId
	ContentLength  int64
}

func unmarshalMediaHeader(b []byte) (MediaHeader, error) {
	var h MediaHeader
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fMediaHdrHeaderID && typ == protowire.VarintType:
			h.HeaderID = uint32(r.varint())
		case num == fMediaHdrItag && typ == protowire.VarintType:
			h.Itag = int32(r.varint())
		case num == fMediaHdrLastModified && typ == protowire.VarintType:
			h.LastModified = r.varint()
		case num == fMediaHdrXTags && typ == protowire.BytesType:
			h.XTags = r.string()
		case num == fMediaHdrIsInitSeg && typ == protowire.VarintType:
			h.IsInitSeg = r.varint() != 0
		case num == fMediaHdrSequenceNum && typ == protowire.VarintType:
			h.SequenceNumber = r.varint()
		case num == fMediaHdrStartMs && typ == protowire.VarintType:
			h.StartMs = int64(r.varint())
		case num == fMediaHdrDurationMs && typ == protowire.VarintType:
			h.DurationMs = int64(r.varint())
		case num == fMediaHdrFormatID && typ == protowire.BytesType:
			fid, err := unmarshalFormatId(r.bytes())
			if err != nil {
				return h, err
			}
			h.FormatId = fid
		case num == fMediaHdrContentLength && typ == protowire.VarintType:
			h.ContentLength = int64(r.varint())
		default:
			r.skip(num, typ)
		}
	}
	return h, r.err
}

// FormatInitializationMetadata (UMP part 42) carries the total segment count and
// init/index byte ranges for a format.
type FormatInitializationMetadata struct {
	FormatId          FormatId
	EndSegmentNumber  int64
	MimeType          string
	InitRange         ByteRange
	IndexRange        ByteRange
	DurationUnits     int64
	DurationTimescale int64
}

func unmarshalFormatInitMetadata(b []byte) (FormatInitializationMetadata, error) {
	var m FormatInitializationMetadata
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fFmtInitFormatID && typ == protowire.BytesType:
			fid, err := unmarshalFormatId(r.bytes())
			if err != nil {
				return m, err
			}
			m.FormatId = fid
		case num == fFmtInitEndSegment && typ == protowire.VarintType:
			m.EndSegmentNumber = int64(r.varint())
		case num == fFmtInitMimeType && typ == protowire.BytesType:
			m.MimeType = r.string()
		case num == fFmtInitInitRange && typ == protowire.BytesType:
			rng, err := unmarshalByteRange(r.bytes())
			if err != nil {
				return m, err
			}
			m.InitRange = rng
		case num == fFmtInitIndexRange && typ == protowire.BytesType:
			rng, err := unmarshalByteRange(r.bytes())
			if err != nil {
				return m, err
			}
			m.IndexRange = rng
		case num == fFmtInitDurationUnits && typ == protowire.VarintType:
			m.DurationUnits = int64(r.varint())
		case num == fFmtInitDurationScale && typ == protowire.VarintType:
			m.DurationTimescale = int64(r.varint())
		default:
			r.skip(num, typ)
		}
	}
	return m, r.err
}

// NextRequestPolicy (UMP part 35) carries the server-directed backoff and the
// playback cookie to echo on the next request.
type NextRequestPolicy struct {
	TargetAudioReadaheadMs int64
	BackoffTimeMs          int64
	PlaybackCookie         []byte
}

func unmarshalNextRequestPolicy(b []byte) (NextRequestPolicy, error) {
	var p NextRequestPolicy
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fNextPolicyReadaheadMs && typ == protowire.VarintType:
			p.TargetAudioReadaheadMs = int64(r.varint())
		case num == fNextPolicyBackoffMs && typ == protowire.VarintType:
			p.BackoffTimeMs = int64(r.varint())
		case num == fNextPolicyCookie && typ == protowire.BytesType:
			p.PlaybackCookie = r.bytesCopy()
		default:
			r.skip(num, typ)
		}
	}
	return p, r.err
}

// SabrRedirect (UMP part 43) points subsequent requests at a new endpoint.
type SabrRedirect struct {
	URL string
}

func unmarshalSabrRedirect(b []byte) (SabrRedirect, error) {
	var s SabrRedirect
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fSabrRedirectURL && typ == protowire.BytesType:
			s.URL = r.string()
		default:
			r.skip(num, typ)
		}
	}
	return s, r.err
}

// SabrError (UMP part 44) is a terminal protocol error from the server. Type is
// a namespaced string on the wire (e.g. "sabr.malformed_config"), not an enum.
type SabrError struct {
	Type string
	Code int32
}

func unmarshalSabrError(b []byte) (SabrError, error) {
	var s SabrError
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fSabrErrorType && typ == protowire.BytesType:
			s.Type = r.string()
		case num == fSabrErrorCode && typ == protowire.VarintType:
			s.Code = int32(r.varint())
		default:
			r.skip(num, typ)
		}
	}
	return s, r.err
}

// StreamProtectionStatus (UMP part 58) reports attestation state: 1=OK,
// 2=ATTESTATION_PENDING, 3=ATTESTATION_REQUIRED.
type StreamProtectionStatus struct {
	Status     int32
	MaxRetries int32
}

func unmarshalStreamProtectionStatus(b []byte) (StreamProtectionStatus, error) {
	var s StreamProtectionStatus
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fProtStatusStatus && typ == protowire.VarintType:
			s.Status = int32(r.varint())
		case num == fProtStatusMaxRetries && typ == protowire.VarintType:
			s.MaxRetries = int32(r.varint())
		default:
			r.skip(num, typ)
		}
	}
	return s, r.err
}

// SabrContextUpdate (UMP part 57) is a context blob the server tells the client
// to echo back in subsequent requests' streamerContext. A later update with the
// same Type replaces the earlier value unless WritePolicy is KEEP_EXISTING.
// HasType distinguishes an absent type from a literal 0.
type SabrContextUpdate struct {
	Type          int32
	HasType       bool
	Scope         int32
	Value         []byte
	SendByDefault bool
	WritePolicy   int32
}

func unmarshalSabrContextUpdate(b []byte) (SabrContextUpdate, error) {
	var u SabrContextUpdate
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		switch {
		case num == fSabrCtxUpdateType && typ == protowire.VarintType:
			u.Type = int32(r.varint())
			u.HasType = true
		case num == fSabrCtxUpdateScope && typ == protowire.VarintType:
			u.Scope = int32(r.varint())
		case num == fSabrCtxUpdateValue && typ == protowire.BytesType:
			// The value is echoed in later requests, so it must not alias the
			// round body (which is reused); copy it. Same reason PlaybackCookie
			// uses bytesCopy.
			u.Value = r.bytesCopy()
		case num == fSabrCtxUpdateSendByDefault && typ == protowire.VarintType:
			u.SendByDefault = r.varint() != 0
		case num == fSabrCtxUpdateWritePolicy && typ == protowire.VarintType:
			u.WritePolicy = int32(r.varint())
		default:
			r.skip(num, typ)
		}
	}
	return u, r.err
}

// SabrContextSendingPolicy (UMP part 59) lists context types to start sending,
// stop sending, or discard. Each field is a repeated int32 that may arrive
// packed or unpacked.
type SabrContextSendingPolicy struct {
	StartPolicy   []int32
	StopPolicy    []int32
	DiscardPolicy []int32
}

func unmarshalSabrContextSendingPolicy(b []byte) (SabrContextSendingPolicy, error) {
	var p SabrContextSendingPolicy
	r := fieldReader{b: b}
	for {
		num, typ, ok := r.next()
		if !ok {
			break
		}
		packable := typ == protowire.VarintType || typ == protowire.BytesType
		switch {
		case num == fSabrSendPolStart && packable:
			p.StartPolicy = r.readPackedInt32s(typ, p.StartPolicy)
		case num == fSabrSendPolStop && packable:
			p.StopPolicy = r.readPackedInt32s(typ, p.StopPolicy)
		case num == fSabrSendPolDiscard && packable:
			p.DiscardPolicy = r.readPackedInt32s(typ, p.DiscardPolicy)
		default:
			r.skip(num, typ)
		}
	}
	return p, r.err
}

// Low-level wire helpers.

func appendVarint(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func appendBytes(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func appendString(b []byte, num protowire.Number, v string) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, v)
}

// fieldReader walks protobuf fields in a buffer. The value accessors and skip
// record the first error; callers loop on next() and check err afterward.
type fieldReader struct {
	b   []byte
	err error
}

func (r *fieldReader) next() (protowire.Number, protowire.Type, bool) {
	if r.err != nil || len(r.b) == 0 {
		return 0, 0, false
	}
	num, typ, n := protowire.ConsumeTag(r.b)
	if n < 0 {
		r.err = protowire.ParseError(n)
		return 0, 0, false
	}
	r.b = r.b[n:]
	return num, typ, true
}

func (r *fieldReader) varint() uint64 {
	v, n := protowire.ConsumeVarint(r.b)
	if n < 0 {
		r.err = protowire.ParseError(n)
		return 0
	}
	r.b = r.b[n:]
	return v
}

// bytes returns a slice backed by the input buffer. Use bytesCopy when retaining
// the value.
func (r *fieldReader) bytes() []byte {
	v, n := protowire.ConsumeBytes(r.b)
	if n < 0 {
		r.err = protowire.ParseError(n)
		return nil
	}
	r.b = r.b[n:]
	return v
}

func (r *fieldReader) bytesCopy() []byte {
	v := r.bytes()
	if len(v) == 0 {
		return nil
	}
	return append([]byte(nil), v...)
}

// readPackedInt32s decodes a repeated int32 field in either form: unpacked
// (VarintType, one value per occurrence) or packed (BytesType, a length-delimited
// run of varints). Decoded values are appended to dst. The caller must only pass
// VarintType or BytesType.
func (r *fieldReader) readPackedInt32s(typ protowire.Type, dst []int32) []int32 {
	if typ == protowire.VarintType {
		return append(dst, int32(r.varint()))
	}
	packed := r.bytes()
	for len(packed) > 0 {
		v, n := protowire.ConsumeVarint(packed)
		if n < 0 {
			r.err = protowire.ParseError(n)
			return dst
		}
		dst = append(dst, int32(v))
		packed = packed[n:]
	}
	return dst
}

func (r *fieldReader) string() string {
	v, n := protowire.ConsumeString(r.b)
	if n < 0 {
		r.err = protowire.ParseError(n)
		return ""
	}
	r.b = r.b[n:]
	return v
}

func (r *fieldReader) skip(num protowire.Number, typ protowire.Type) {
	n := protowire.ConsumeFieldValue(num, typ, r.b)
	if n < 0 {
		r.err = protowire.ParseError(n)
		return
	}
	r.b = r.b[n:]
}
