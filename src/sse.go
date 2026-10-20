package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// remotePortFromAddr extracts the peer's ephemeral TCP port from an
// http.Request RemoteAddr ("127.0.0.1:50741", or "[::1]:50741"). Returns 0 if
// unparseable. That port uniquely identifies the connection's socket, which
// native.bundle_for_remote_port maps to the owning process's bundle. On
// Windows the connection reaches us through the platform's listener relay,
// not a socket we accepted; the SDK's relay listener reports the client's
// address (carried in the relay's answer line) as RemoteAddr, so the port
// here is the browser's there too.
func remotePortFromAddr(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// resolveAssertedBundle asks native for the bundle of the process that owns this
// connection's socket and records it under conn_id. Runs OFF the SSE setup path
// (it spawns an lsof + one RPC) so it never delays the stream. Guarded on the
// conn still mapping to this client, so a reconnect race can't overwrite a newer
// assertion with a stale one. Sets the dispatch-scoping bundle for this client
// and recomputes the focused source — the assertion may be the signal that first
// identifies the frontmost browser (its window claimed focus before its identity
// resolved), in which case the recompute arms it.
func (h *Host) resolveAssertedBundle(pluginID, connID, clientID string, remotePort int) {
	res, err := h.plugin.NativeBundleForRemotePort(branchkit.NativeBundleForRemotePortRequest{RemotePort: remotePort})
	if err != nil || res.BundleID == "" {
		branchkit.Logf(pluginID, "[IDENTITY] conn=%s port=%d unresolved: %v", connID, remotePort, err)
		return
	}

	h.mu.Lock()
	stillCurrent := connID != "" && h.state.clientOf(connID) == clientID
	var armBundle, armVis string
	var needsRescan bool
	if stillCurrent {
		h.state.conn(connID).AssertedBundle = res.BundleID
		// Dispatch scoping reads SSEClientBundles; keep it in lockstep with the
		// asserted identity for every live client (decoupled from focus).
		h.state.SSEClientBundles[clientID] = res.BundleID
		// Record the bundle as connection-capable and close any disconnect
		// episode for it (stale-disconnect tripwire, disconnect_nudge.go).
		h.noteBundleConnectedLocked(res.BundleID)
		wasFocused := h.state.BrowserFocused
		var isBrowser bool
		isBrowser, needsRescan = h.recomputeFocusedSourceLocked("asserted_resolved")
		if isBrowser && !wasFocused {
			armBundle = h.state.FocusedBrowserBundleID
			armVis = h.state.HintVisibility
		}
	}
	h.mu.Unlock()

	if !stillCurrent {
		return
	}
	branchkit.Logf(pluginID, "[IDENTITY] conn=%s port=%d asserted_bundle=%q", connID, remotePort, res.BundleID)
	if needsRescan {
		h.pushRescanToClients()
	}
	if armBundle != "" {
		h.armBrowserFocusUnlocked(pluginID, armBundle, armVis)
	}
}

// Why a focus-scoped event reached no browser. Carried back to the platform on
// the on_action reply (handleOnAction) so a spoken command that cannot run is
// an error with a reason, not a silent "ok".
const (
	// No extension connection at all, or the frontmost browser is one whose
	// extension held a connection and no longer does (seen_browsers.go).
	dispatchNoBrowser = "browser_not_connected"
	// Browsers are connected, but none of them is the app in front.
	dispatchNotFocused = "browser_not_focused"
	// Several connections could be the one in front and nothing says which:
	// focus unknown (before the first focus event) or identities unresolved.
	// Refusing beats acting in every one of them.
	dispatchAmbiguous = "browser_ambiguous"
	// The target's event channel is full — its reader is wedged or backed up.
	dispatchBusy = "browser_busy"
)

// focusedClientLocked picks the ONE SSE client a focus-scoped event goes to:
// the client of FocusedConnID, the connection the recompute bound to the
// frontmost bundle (and, among profiles sharing a bundle, the one that last
// claimed window focus). Returns "" and a dispatch* reason when there is none.
//
// With no bound focused connection it fails open only to a connection that
// MIGHT be the one in front and is the only such: any connection while focus
// is still unknown, otherwise only connections whose identity never resolved
// (a resolved identity naming a different bundle is positively NOT in front).
// That keeps a lone browser working before the focus handshake binds, and on a
// host whose identity lookup is unavailable.
//
// It used to broadcast to every client whenever nothing matched. With Chrome
// in front and its extension gone, "close tab" then closed the active tab in
// Firefox behind it; with two Chrome profiles, it ran in both. A command now
// reaches exactly one browser or none, and none comes back as an error.
// Must be called with `mu` held.
func (h *Host) focusedClientLocked() (string, string) {
	if conn := h.state.FocusedConnID; conn != "" {
		if client := h.state.clientOf(conn); client != "" {
			if _, live := h.state.SSEClients[client]; live {
				return client, ""
			}
		}
		return "", dispatchNoBrowser
	}
	if len(h.state.SSEClients) == 0 {
		return "", dispatchNoBrowser
	}
	front := h.state.FocusedBundleID
	candidate, n := "", 0
	for id := range h.state.SSEClients {
		if b := h.state.SSEClientBundles[id]; front != "" && b != "" && b != front {
			continue
		}
		candidate = id
		n++
	}
	switch {
	case n == 1:
		return candidate, ""
	case n > 1:
		return "", dispatchAmbiguous
	case h.state.SeenBundles[front]:
		return "", dispatchNoBrowser
	default:
		return "", dispatchNotFocused
	}
}

// sendToClientLocked queues one event on one client's stream. False when the
// channel is full: the event (possibly a user's activate dispatch) is LOST.
// Logged — rare by construction, and a silent drop here cost a debugging
// session when a spoken command did nothing. Must be called with `mu` held.
func (h *Host) sendToClientLocked(clientID string, event ActionEvent) bool {
	ch, ok := h.state.SSEClients[clientID]
	if !ok {
		return false
	}
	select {
	case ch <- event:
		return true
	default:
		branchkit.Logf(h.state.PluginID, "[SSE] dropped %q event for client %s: channel full", event.Action, clientID)
		return false
	}
}

// sendToFocusedClientsLocked sends an ActionEvent to the focused browser's
// connection only (focusedClientLocked). With multiple browsers connected, a
// spoken command resolves against the focused browser's grammar — so reaching
// any other browser would let it act on a stale badge that happens to share
// the codeword. Returns how many clients received it (0 or 1) and, when 0,
// the dispatch* reason. Must be called with `mu` held.
func (h *Host) sendToFocusedClientsLocked(event ActionEvent) (int, string) {
	client, reason := h.focusedClientLocked()
	if client == "" {
		return 0, reason
	}
	if !h.sendToClientLocked(client, event) {
		return 0, dispatchBusy
	}
	return 1, ""
}

// sendToConnsLocked sends an event to each named connection's client (media
// verbs, which follow the playing browser rather than focus). Returns how many
// received it and, when none did, why. Must be called with `mu` held.
func (h *Host) sendToConnsLocked(conns []string, event ActionEvent) (int, string) {
	sent := 0
	reason := dispatchNoBrowser
	for _, conn := range conns {
		client := h.state.clientOf(conn)
		if client == "" {
			continue
		}
		if h.sendToClientLocked(client, event) {
			sent++
		} else {
			reason = dispatchBusy
		}
	}
	if sent > 0 {
		return sent, ""
	}
	return 0, reason
}

// broadcastToClientsLocked sends an event to every connected client. Only for
// events that are a reset, not an action — a background browser receiving one
// is correct, not a leak. Must be called with `mu` held.
func (h *Host) broadcastToClientsLocked(event ActionEvent) {
	for id := range h.state.SSEClients {
		h.sendToClientLocked(id, event)
	}
}

// handleSSE holds an SSE connection open to the browser extension.
// When a voice command matches (via on_action hook), the action is pushed here.
func (h *Host) handleSSE(w http.ResponseWriter, r *http.Request, connID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Register this client. connID (the stream URL's conn_id) is the
	// extension's stable connection nonce (it can't reliably name its own
	// browser bundle — UA-sniffing breaks for forks). We map it to this SSE
	// channel so dispatch can reach the right connection; its bundle identity
	// is resolved from the peer socket just below (resolveAssertedBundle).
	// EventSource can't send custom headers, hence the query param.
	remotePort := remotePortFromAddr(r.RemoteAddr)
	pluginID, clientID, ch := h.registerSSEClient(connID)
	alphabetCh := h.alphabetChannel(clientID)

	// Seed the new client with the owner's current badge mode. hint_display
	// lives in the settings collection; the extension keeps a chrome.storage
	// cache of it so badges paint with the host down. Sending it on connect is
	// what makes a fresh profile (or one whose cache drifted) converge on the
	// collection instead of on its own local default. Buffered channel, so this
	// cannot block the handshake.
	if conf := h.LoadBrowserConfig(); conf.HintDisplay != "" {
		select {
		case ch <- wire.NewAction("set_badge_mode", wire.SetBadgeModeParams{Mode: conf.HintDisplay}):
		default:
		}
	}

	// Assert this connection's bundle from the OS (peer socket -> owning process
	// -> bundle), off the setup path so the stream isn't delayed. This is the sole
	// identity source; it also sets SSEClientBundles for dispatch scoping. Focus-
	// independent ground truth.
	if connID != "" && remotePort > 0 {
		go h.resolveAssertedBundle(pluginID, connID, clientID, remotePort)
	}

	defer h.disconnectSSEClient(pluginID, connID, clientID)

	// The connected event carries this side's wire version (the ticket already
	// refused a mismatch; this lets the extension log what it is talking to).
	writeSSE(w, "connected", wire.ConnectedEvent{WireVersion: wire.Version()})
	flusher.Flush()

	// Push the alphabet so the extension labels hint badges with the same
	// codewords the voice plugin uses for matching. Without this, badges fall
	// back to the extension's built-in HINT_WORDS list and would disagree with
	// what voice recognizes. initCodewords here covers the cold-start case:
	// the SSE connect can fire before any grammar push has run. A later
	// change arrives on alphabetCh (applyAlphabet); the stream was registered
	// above, so a change racing this snapshot is caught by one or the other.
	h.refreshCodewords()
	if words := h.snapshotCodewords(); words != nil {
		writeSSE(w, "alphabet", wire.AlphabetEvent{Words: words})
		flusher.Flush()
	}

	// A fresh SSE stream means our per-frame sessions are gone: either this is
	// a cold connect, or the prior connection's teardown ran
	// clearHintsStateOnDisconnect. The extension can't know that — each frame
	// skips a grammar send whose content the plugin already accepted, so
	// nothing would go out and those codewords would stay
	// painted-but-unmatchable. Tell it to `reactivate`, which re-sends every
	// frame's grammar whatever it last sent. This is what makes the
	// badge-visible-implies-commandable invariant hold across a plugin restart
	// or reconnect, deterministically and on every engine, since the connect
	// action is the one signal the client reliably sees after a session reset.
	writeSSE(w, "action", wire.NewAction("reactivate", wire.ReactivateParams{Reason: "sse_connect"}))
	flusher.Flush()

	// Stream events until client disconnects
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-ch:
			writeSSE(w, "action", event)
			flusher.Flush()
		case words := <-alphabetCh:
			writeSSE(w, "alphabet", wire.AlphabetEvent{Words: words})
			flusher.Flush()
		}
	}
}

// writeSSE writes one named event; data is one of wire.StreamEvents' types.
func writeSSE(w http.ResponseWriter, name string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
}

// registerSSEClient adds a stream's channel to the client set and maps its
// conn_id to it. A second stream for a conn_id already mapped (a same-conn_id
// reconnect) takes the mapping over; the old stream's disconnectSSEClient then
// sees it no longer owns the conn and leaves the conn's state alone.
func (h *Host) registerSSEClient(connID string) (pluginID, clientID string, ch chan ActionEvent) {
	ch = make(chan ActionEvent, 16)
	clientID = fmt.Sprintf("%p", ch)
	h.mu.Lock()
	defer h.mu.Unlock()
	pluginID = h.state.PluginID
	h.state.SSEClients[clientID] = ch
	if connID != "" {
		h.state.conn(connID).Client = clientID
	}
	return pluginID, clientID, ch
}

// alphabetChannel registers a stream's alphabet channel (applyAlphabet pushes
// a changed alphabet on it). Dropped with the stream in disconnectSSEClient.
func (h *Host) alphabetChannel(clientID string) chan []string {
	ch := make(chan []string, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.SSEAlphabet == nil {
		h.state.SSEAlphabet = make(map[string]chan []string)
	}
	h.state.SSEAlphabet[clientID] = ch
	return ch
}

// disconnectSSEClient tears down one ended stream: its client entry always,
// and its connection's state only while the stream still owns the conn_id.
func (h *Host) disconnectSSEClient(pluginID, connID, clientID string) {
	droppedContrib := false
	var armBundle, armVis, lostBundle string
	var needsRescan, focusLost, pinCleared bool
	h.mu.Lock()
	delete(h.state.SSEClients, clientID)
	delete(h.state.SSEAlphabet, clientID)
	delete(h.state.SSEClientBundles, clientID)
	// Everything below is keyed by conn_id, and a conn_id can outlive this
	// stream: the extension reconnects under the SAME conn_id (a missed
	// health probe, the fresh-credentials path) and the new stream
	// registers before this one's context ends. Only the stream that still
	// owns the conn may tear its state down. The map deletes always
	// checked this; the rest did not, so a same-conn_id reconnect's old
	// stream deleted the command catalog the new stream had just
	// contributed — scroll, find and navigation commands vanished until
	// the next reconnect.
	owned := connID != "" && h.state.clientOf(connID) == clientID
	if owned {
		wasFocused := h.state.FocusedConnID == connID
		// The connection's own state goes in one place: its Conn — stream,
		// focus claim, active tab, focused window, asserted bundle (cheap to
		// re-derive on the next accept, and dropping it keeps the set of
		// asserted conns the live-browser set the recompute relies on), tab
		// words, mode session. What follows is the effect of that drop on what
		// it projected elsewhere: tags and collections.
		hadTabs := h.state.peek(connID).TabsPublished
		delete(h.state.Conns, connID)
		// A pin (focus_pin.go) never outlives its connection: focus goes back
		// to what the OS reports, and the recompute below applies it.
		if h.pin.clearIf(connID) {
			pinCleared = true
			h.restoreOSFrontmostLocked()
		}
		// If this conn held the focused slot, release it, so the slot never
		// points at a dead conn — that would store live batches as
		// "non-focused" and project the per-prefix collections empty
		// (codewords fall through to letter keys). The recompute below
		// decides who holds it next.
		if wasFocused {
			h.state.FocusedConnID = ""
			h.state.FocusedTabID = 0
			h.state.BrowserFocused = false
		}
		// Re-project the tab words without this connection's (the focused
		// slot was released above if it was ours, so the sync falls back to
		// the lone survivor or drains). A drain/handoff never adds engine
		// words, so no vocabulary commit is needed here.
		if hadTabs {
			h.syncTabsCollectionLocked(pluginID)
		}
		// A dead publisher can't send its "off": release every mode slot it
		// holds (caret, find, video, keep-scrolling, query field — each
		// against its OWN publisher; video used to drain only inside the caret
		// check), and re-derive the media tag without its
		// assertion (the background re-sends its session on reconnect, so the
		// drop can't strand it).
		h.releaseConnSlotsLocked(pluginID, connID, "sse_disconnect")
		h.syncMediaActiveLocked(pluginID)
		// Likewise the chip window (range_pick.go): a dead publisher can't
		// send its release, and a stranded pick would keep that tab's hints
		// filtered out of the projection — and so out of the HUD.
		if h.state.RangePickConnID == connID {
			h.drainRangePickLocked(pluginID, "sse_disconnect")
		}
		// A dead publisher can't send its palette close — drain here so
		// the exclusive tag never outlives the extension that set it.
		if h.state.PaletteConnID == connID {
			h.drainPaletteLocked(pluginID, "sse_disconnect")
		}
		// Forget this connection's contributed command catalog, so a
		// closed browser's commands don't outlive it in the union. The
		// re-push happens after the lock is released (pushCommandGroup
		// takes its own).
		droppedContrib = h.dropContributedCommands(connID)
		// This conn's frame sessions can never be revived — a reconnect that
		// is not this same stream's conn_id mints a fresh one — so destroy
		// them and drain what they projected. Nothing called this before:
		// every service-worker restart left its predecessor's sessions in
		// memory for the plugin's lifetime (iterated under the lock on every
		// batch), and their projected codewords stayed matchable, so a stale
		// pair completed `activate` into "no such hint". Runs before the
		// recompute below so a handover finds nothing of this conn to park.
		h.cleanupConnSessionsLocked(pluginID, connID, "sse_disconnect")
		// The focused browser lost its connection. Another connection may
		// hold the same bundle (a second profile) and take over; otherwise
		// the app in front is a browser with no extension, and the
		// browser-scoped tags must go — the lone-publisher fallbacks
		// would otherwise keep "close tab" matching against a background
		// browser.
		if wasFocused {
			var isBrowser bool
			isBrowser, needsRescan = h.recomputeFocusedSourceLocked("sse_disconnect")
			if isBrowser {
				armBundle = h.state.FocusedBrowserBundleID
				armVis = h.state.HintVisibility
			} else {
				focusLost = true
				lostBundle = h.state.FocusedBundleID
				// The user is looking at a browser whose extension just went.
				// Arm the stale-disconnect nudge now: it used to arm only on
				// a LATER app-focus event, so with the browser staying in
				// front nothing was ever said. The grace period absorbs a
				// routine reconnect (a service-worker restart), which cancels
				// it.
				h.armDisconnectNudgeLocked(lostBundle)
			}
		}
	}
	lastClient := len(h.state.SSEClients) == 0
	h.mu.Unlock()
	if pinCleared {
		branchkit.Logf(pluginID, "[FOCUS-PIN] pinned conn=%q disconnected; pin cleared, focus follows the OS again", connID)
	}
	// Outside the lock: pushCommandGroup takes its own.
	if droppedContrib {
		if err := h.pushCommandGroup(pluginID, groupContributed); err != nil {
			branchkit.Logf(pluginID, "contribute drain: push failed: %v", err)
		}
	}
	if needsRescan {
		h.pushRescanToClients()
	}
	if armBundle != "" {
		branchkit.Logf(pluginID, "[FOCUS] focused conn=%q disconnected; %q focus passes to conn=%q", connID, armBundle, h.focusedConnSnapshot())
		h.armBrowserFocusUnlocked(pluginID, armBundle, armVis)
	}
	if focusLost {
		branchkit.Logf(pluginID, "[FOCUS] focused conn=%q disconnected; no connection left for %q", connID, lostBundle)
		h.releaseBrowserFocusUnlocked(pluginID, lostBundle, "sse_disconnect conn="+connID)
	}
	if lastClient {
		h.clearHintsStateOnDisconnect(h.state.PluginID)
	}
}
