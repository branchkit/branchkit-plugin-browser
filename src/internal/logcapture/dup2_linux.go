package logcapture

import "syscall"

// dup2 via dup3: linux/arm64 has no dup2 system call.
func dup2(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }
