//go:build unix

package ops

import "syscall"

// access(2) modes.
const (
	modeWrite = 0x2
	modeExec  = 0x1
)

// access asks the kernel whether this process may use path with mode, which
// honours read-only mounts and ACLs that a mode-bit test would miss.
func access(path string, mode uint32) error { return syscall.Access(path, mode) }
