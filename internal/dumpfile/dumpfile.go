// Package dumpfile writes timestamped diagnostic artifacts. It is the shared
// core of the WAXTAP_DUMP_DIR and SABR dump paths: callers gate it on
// configuration, log the returned error at debug, and otherwise ignore it, so
// diagnostics never affect the operation being diagnosed.
package dumpfile

import (
	"os"
	"path/filepath"
	"time"
)

// Write stores data under dir as "<UTC timestamp>-<name>", creating dir if
// needed. It returns the path written; on error the path identifies the
// attempted file when the write itself failed, or is empty when the directory
// could not be created.
func Write(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, time.Now().UTC().Format("20060102T150405.000Z")+"-"+name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return path, err
	}
	return path, nil
}
