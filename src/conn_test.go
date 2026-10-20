package main

// conns builds PluginState.Conns for a test, one Conn per conn_id.
func conns(cs map[string]Conn) map[string]*Conn {
	out := make(map[string]*Conn, len(cs))
	for id, c := range cs {
		c := c
		out[id] = &c
	}
	return out
}

// windowConns builds Conns whose only state is each one's focused window.
func windowConns(windows map[string]int) map[string]*Conn {
	cs := map[string]Conn{}
	for id, w := range windows {
		cs[id] = Conn{FocusedWindow: w}
	}
	return conns(cs)
}

// setTabs records a connection's published tab words, as POST /tabs does.
func (s *PluginState) setTabs(id string, entries []TabEntry) {
	c := s.conn(id)
	c.TabEntries, c.TabsPublished = entries, true
}
