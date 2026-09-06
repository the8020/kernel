package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

const writerLockName = "writer.lock"

// WriterActive lets the kernel's standby raw readers avoid consuming bytes
// while a previous logd still owns and drains the node. It creates no files.
func WriterActive(directory string) (bool, error) {
	file, err := os.OpenFile(filepath.Join(directory, writerLockName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return false, errors.New("log writer lock is not a regular file")
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, err
}
