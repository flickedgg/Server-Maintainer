package core

import (
	"errors"
	"fmt"
	"syscall"
)

const errSharingViolation syscall.Errno = 32

// AcquireLock opens path with no sharing, so a second maintainer can't open it.
// Windows releases the handle if the process dies.
func AcquireLock(path string) (release func(), err error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, errSharingViolation) {
		return nil, fmt.Errorf("another maintainer is already running for this config (lock %s is held)", path)
	}
	if err != nil {
		return nil, err
	}
	return func() { syscall.CloseHandle(h) }, nil
}
