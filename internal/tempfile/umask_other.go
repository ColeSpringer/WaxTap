//go:build !unix

package tempfile

import "os"

// currentUmask returns 0 on platforms without a POSIX umask, so regular output
// files use mode 0666.
func currentUmask() os.FileMode { return 0 }
