//go:build !unix

package ops

import (
	"errors"
	"os"
)

const (
	modeWrite = 0x2
	modeExec  = 0x1
)

// access is the portable approximation: the permission bits.
func access(path string, mode uint32) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&os.FileMode(mode<<6) == 0 {
		return errors.New("permission denied")
	}
	return nil
}
