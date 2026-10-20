package logcapture

import "os"

// redirect assigns os.Stderr: Windows cannot move the handle an open *os.File
// already holds. A goroutine logging at the same moment races this write;
// the Windows CI leg runs without -race, and the unix path has no such race.
func redirect(w *os.File) (restore func(), err error) {
	orig := os.Stderr
	os.Stderr = w
	return func() { os.Stderr = orig }, nil
}
