package wire

import (
	"regexp"
	"strings"
	"testing"
)

// A doc edit must not change the protocol's identity: the ticket refuses a
// version mismatch, so an extension built before a comment changed would be
// shut out though the wire is byte for byte the same.
func TestVersionIgnoresDocs(t *testing.T) {
	before := versionOf(schema(Docs{}))

	i := extensionRoute(t)
	saved := Routes[i].Doc
	Routes[i].Doc = saved + "\nAn added line.\n\nAnd a paragraph."
	savedEvents := Events.Doc
	Events.Doc = "A different doc."
	defer func() { Routes[i].Doc, Events.Doc = saved, savedEvents }()

	docs := Docs{Types: map[string]string{}, Fields: map[string]string{}}
	for _, m := range regexp.MustCompile(`(?m)^export (?:interface|type) (\w+)`).FindAllStringSubmatch(schema(Docs{}), -1) {
		docs.Types[m[1]] = "A type doc."
	}
	if len(docs.Types) == 0 {
		t.Fatal("found no declarations to document")
	}
	if got := versionOf(schema(docs)); got != before {
		t.Fatalf("a doc edit changed the wire version: %s -> %s", before, got)
	}
}

// The hash still sees the declarations.
func TestVersionSeesDeclarations(t *testing.T) {
	bare := withoutComments(schema(Docs{}))
	for _, r := range Routes {
		if !r.Dev && !r.Stream() && !strings.Contains(bare, "'"+r.Path+"'") {
			t.Fatalf("route %s is missing from the hashed schema", r.Path)
		}
	}
	i := extensionRoute(t)
	saved := Routes[i].Path
	Routes[i].Path = saved + "-renamed"
	defer func() { Routes[i].Path = saved }()
	if versionOf(schema(Docs{})) == versionOf(withoutComments(bare)) {
		t.Fatal("renaming a route did not change the wire version")
	}
}

// extensionRoute is the index of the first route the schema writes.
func extensionRoute(t *testing.T) int {
	for i, r := range Routes {
		if !r.Dev && !r.Stream() {
			return i
		}
	}
	t.Fatal("no extension-facing route")
	return 0
}
