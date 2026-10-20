// wiregen writes the extension's src/wire/gen.ts from the wire package: the
// routes, their request and reply types, the stream's events and the
// synthetic actions, with the package's doc comments.
//
//	go run ./cmd/wiregen              # gen.ts on stdout
//	go run ./cmd/wiregen -o PATH      # write it
//	go run ./cmd/wiregen -version     # just WIRE_VERSION
//
// Run from plugins/browser/src. The app's `just check-browser-wire`
// regenerates and diffs against the extension's committed file.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/branchkit-plugin-browser/wire/wiredoc"
)

func main() {
	src := flag.String("src", "wire", "the wire package's source directory (for doc comments)")
	out := flag.String("o", "", "write to this path instead of stdout")
	versionOnly := flag.Bool("version", false, "print WIRE_VERSION and exit")
	flag.Parse()

	if *versionOnly {
		fmt.Println(wire.Version())
		return
	}
	docs, err := wiredoc.Read(*src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wiregen:", err)
		os.Exit(1)
	}
	ts := wire.TypeScript(docs)
	if *out == "" {
		fmt.Print(ts)
		return
	}
	if err := os.WriteFile(*out, []byte(ts), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "wiregen:", err)
		os.Exit(1)
	}
}
