// Package logcapture captures what browser writes to stderr, for tests.
//
// branchkit.Logf writes to os.Stderr, and the platform turns plugin stderr
// into the plugin log — so stderr is where a test looks for words that must
// not reach a log. A copy of voice's package of the same name: an internal
// package cannot be imported across modules.
//
// It redirects the descriptor os.Stderr holds rather than assigning
// os.Stderr. The variable is read by every goroutine that logs, including
// timers other tests leave running, so assigning it is a data race that
// `go test -race` reports. Moving the descriptor underneath it is invisible
// to the race detector and catches the same writes. Windows has no dup2 for
// an open *os.File, so there it assigns the variable; CI runs the Windows
// leg without -race.
package logcapture

import (
	"io"
	"os"
	"sync"
)

// mu serializes captures: one process, one stderr.
var mu sync.Mutex

// Stderr runs fn and returns everything written to stderr while it ran, by
// any goroutine.
func Stderr(fn func()) (string, error) {
	mu.Lock()
	defer mu.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()

	restore, err := redirect(w)
	if err != nil {
		w.Close()
		<-out
		r.Close()
		return "", err
	}
	func() {
		defer restore()
		fn()
	}()
	// The pipe reaches EOF only once every write end is closed: w here, and
	// the descriptor redirect pointed at it, which restore put back.
	w.Close()
	got := <-out
	r.Close()
	return got, nil
}
