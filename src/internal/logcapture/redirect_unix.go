//go:build !windows

package logcapture

import (
	"os"
	"syscall"
)

// redirect points the stderr descriptor at w and returns what puts it back.
func redirect(w *os.File) (restore func(), err error) {
	// The descriptor os.Stderr holds, not a literal 2: under `go test -json`
	// the testing package sets os.Stderr = os.Stdout so test2json sees one
	// ordered stream, and branchkit.Logf then writes to descriptor 1.
	stderr := int(os.Stderr.Fd())
	saved, err := syscall.Dup(stderr)
	if err != nil {
		return nil, err
	}
	if err := dup2(int(w.Fd()), stderr); err != nil {
		syscall.Close(saved)
		return nil, err
	}
	return func() {
		_ = dup2(saved, stderr)
		syscall.Close(saved)
	}, nil
}
