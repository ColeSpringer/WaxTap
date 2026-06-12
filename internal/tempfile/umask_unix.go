//go:build unix

package tempfile

import (
	"os"
	"syscall"
)

// processUmask is captured during package initialization. POSIX provides no
// read-only umask operation, so caching avoids changing the process-wide value
// while staged files are created concurrently.
var processUmask = func() os.FileMode {
	old := syscall.Umask(0)
	syscall.Umask(old)
	return os.FileMode(old)
}()

// currentUmask returns the umask captured during package initialization.
func currentUmask() os.FileMode { return processUmask }
