//go:build unix

package persistence

import (
	"fmt"
	"os"
	"syscall"
)

// fileLock is an advisory, process-wide exclusive lock on a lock file next to
// the AOF. It stops a second goredis process (for example a second pod during
// a bad rollout, or an operator starting a copy by hand) from appending to the
// same file and interleaving records. The kernel releases it automatically if
// the process dies, so there are no stale locks to clean up.
type fileLock struct{ f *os.File }

func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open AOF lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("AOF lock %s is held by another process (is another goredis running on this data directory?): %w", path, err)
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
