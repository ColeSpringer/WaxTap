package youtube

import (
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	xtagKeyAudioContent      = "acont" // audio role: original, dubbed, dubbed-auto, descriptive, secondary
	xtagAudioContentOriginal = "original"
)

// xtagsAudioContent returns the acont value of the player response's xtags:
// unpadded base64url over a protobuf of repeated pairs (field 1, each {key=1,
// value=2}). The first non-empty acont wins if the key repeats, and unknown
// fields are skipped. It reports false when s carries no acont or is not well
// formed: not base64, truncated, or a key or value that is not UTF-8. The value
// is only ever read: SABR keys renditions on it byte for byte.
func xtagsAudioContent(s string) (string, bool) {
	b, err := decodeBase64Tolerant(s)
	if err != nil {
		return "", false
	}
	var acont string
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", false
		}
		b = b[n:]
		if num != 1 || typ != protowire.BytesType {
			if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
				return "", false
			}
			b = b[n:]
			continue
		}
		pair, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return "", false
		}
		b = b[n:]
		key, value, ok := xtagsPair(pair)
		if !ok {
			return "", false
		}
		if acont == "" && key == xtagKeyAudioContent {
			acont = value
		}
	}
	return acont, acont != ""
}

// xtagsPair decodes one {key=1, value=2} pair; ok is false when it is malformed.
func xtagsPair(b []byte) (key, value string, ok bool) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", "", false
		}
		b = b[n:]
		if (num != 1 && num != 2) || typ != protowire.BytesType {
			if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
				return "", "", false
			}
			b = b[n:]
			continue
		}
		v, n := protowire.ConsumeString(b)
		if n < 0 || !utf8.ValidString(v) {
			return "", "", false
		}
		b = b[n:]
		if num == 1 {
			key = v
		} else {
			value = v
		}
	}
	return key, value, true
}
