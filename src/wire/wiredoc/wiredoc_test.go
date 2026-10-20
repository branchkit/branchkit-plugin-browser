package wiredoc

import (
	"os"
	"testing"

	"github.com/branchkit/branchkit-plugin-browser/wire"
)

// WIRE.md is generated from the wire package and committed beside the
// README, where an extension author finds it; it must say what the code
// does. Regenerate: cd src && go run ./cmd/wiregen -md -o ../WIRE.md
func TestWireMarkdownIsCurrent(t *testing.T) {
	docs, err := Read("..")
	if err != nil {
		t.Fatal(err)
	}
	want := wire.Markdown(docs)
	got, err := os.ReadFile("../../../WIRE.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("WIRE.md is out of date: cd src && go run ./cmd/wiregen -md -o ../WIRE.md")
	}
}
