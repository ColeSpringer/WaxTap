package youtube

import (
	"context"
	"os"

	"github.com/colespringer/waxtap/v2/internal/dumpfile"
)

// dumpEnvVar names the directory for env-gated diagnostic artifacts. When it is
// set, extraction writes unusable raw player responses or watch pages there so a
// maintainer can inspect what YouTube returned. It is off by default and checked
// only on failure paths.
const dumpEnvVar = "WAXTAP_DUMP_DIR"

// sabrDumpEnvVar names the directory for raw SABR round dumps. When it is set,
// every SABR response body is written there so the exact UMP/protobuf bytes
// can be re-decoded offline (see internal/sabr Config.DumpDir). Off by default.
const sabrDumpEnvVar = "WAXTAP_SABR_DUMP_DIR"

// dumpArtifact writes data to a timestamped file under WAXTAP_DUMP_DIR when that
// variable is set. It is best-effort: an unset variable does nothing, and every
// error is logged at debug and otherwise ignored. Diagnostics must never change
// extraction's outcome. The label is a short tag such as
// "playerresponse-WEB-vid.json".
func (c *Client) dumpArtifact(ctx context.Context, label string, data []byte) {
	dir := os.Getenv(dumpEnvVar)
	if dir == "" || len(data) == 0 {
		return
	}
	path, err := dumpfile.Write(dir, sanitizeLabel(label), data)
	if err != nil {
		c.log.DebugContext(ctx, "artifact dump failed", "dir", dir, "err", err)
		return
	}
	c.log.DebugContext(ctx, "wrote diagnostic artifact", "path", path)
}

// sanitizeLabel keeps a label filesystem-safe by replacing anything outside
// [A-Za-z0-9._-] with an underscore.
func sanitizeLabel(s string) string {
	safe := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		out = append(out, safe(r))
	}
	return string(out)
}
