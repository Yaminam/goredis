//go:build windows

package persistence

import (
	"fmt"
	"syscall"
)

// fileLock holds the lock file open with no sharing allowed, so a second
// process cannot open it. Windows releases the handle when the process
// exits, so there are no stale locks to clean up.
type fileLock struct{ h syscall.Handle }

func acquireLock(path string) (*fileLock, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing: this is the lock
		nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("AOF lock %s is held by another process (is another goredis running on this data directory?): %w", path, err)
	}
	return &fileLock{h: h}, nil
}

func (l *fileLock) release() { _ = syscall.CloseHandle(l.h) }
