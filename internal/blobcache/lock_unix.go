//go:build unix

package blobcache

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryFileLock(f *os.File, exclusive bool) (bool, error) {
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	err := unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
