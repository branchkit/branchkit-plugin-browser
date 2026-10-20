package main

import "github.com/branchkit/branchkit-plugin-browser/wire"

// Conn is everything this plugin holds for one extension connection, keyed by
// the extension's conn_id in PluginState.Conns.
//
// It replaces eight parallel maps (ConnToClient, ConnActiveTab,
// ConnFocusedWindow, ConnFocusSeq, ConnClaimAt, ConnClaimSeq, AssertedBundle,
// ConnTabEntries) plus the media slot map. Every one of them had to be
// remembered by the disconnect path, which deleted them one by one; a field
// added here is dropped with the rest by the single `delete(Conns, id)` in
// disconnectSSEClient. What that path still does by hand is the platform-side
// effect of the drop — tags and collections the connection held — because
// those are not state here but writes elsewhere.
//
// Single-slot modes (caret, video, find, ...) stay on PluginState: the tags
// they mirror are global, and the slot names its owner connection. What a
// connection WANTS is Session; the slots are derived from it (session.go).
type Conn struct {
	// The live SSE client id; "" between streams (a write that arrives for a
	// connection with no stream creates the entry, as the maps did).
	Client string

	// The connection's last-reported active tab: a fast path that pre-seeds
	// FocusedTabID on a window switch (see PluginState.FocusedTabID).
	ActiveTab int

	// The chrome.windows id this connection last reported OS-focused (POST
	// /focus): the window-focus arbitration input for grammar batches.
	FocusedWindow int

	// The standing focus claim: its arrival stamp (from PluginState.FocusSeq)
	// and when it was made (epoch ms). Both zero when no claim stands — a
	// focused:false claim releases them.
	FocusSeq uint64
	ClaimAt  int64

	// The seq of the last /focus claim applied from this connection; a claim
	// not newer is dropped.
	ClaimSeq uint64

	// The OS-asserted bundle of the connection's peer process, resolved at
	// stream accept. "" until resolved; the set of conns with one IS the
	// connected-browser set.
	AssertedBundle string

	// The connection's published open-tab words (POST /tabs). TabsPublished
	// distinguishes "published an empty set" from "never published".
	TabEntries    []TabEntry
	TabsPublished bool

	// What the connection's extension last said its modes are (PUT /session).
	// Level state: the single-slot tags are derived from it, and re-derived
	// when focus moves to this connection.
	Session wire.SessionRequest
}

// conn returns the connection's entry, creating it. Must hold mu.
func (s *PluginState) conn(id string) *Conn {
	if s.Conns == nil {
		s.Conns = map[string]*Conn{}
	}
	c := s.Conns[id]
	if c == nil {
		c = &Conn{}
		s.Conns[id] = c
	}
	return c
}

// peek returns a copy of the connection's entry, or the zero Conn. Never
// creates one. Must hold mu.
func (s *PluginState) peek(id string) Conn {
	if c := s.Conns[id]; c != nil {
		return *c
	}
	return Conn{}
}

// clientOf is the connection's live SSE client id, or "". Must hold mu.
func (s *PluginState) clientOf(id string) string {
	if c := s.Conns[id]; c != nil {
		return c.Client
	}
	return ""
}

// liveConnCount is how many connections have a live stream. Must hold mu.
func (s *PluginState) liveConnCount() int {
	n := 0
	for _, c := range s.Conns {
		if c.Client != "" {
			n++
		}
	}
	return n
}
