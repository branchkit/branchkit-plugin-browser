package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// startExtensionListener binds a TCP port for the browser extension to connect to.
// Returns the listen address and pairing token.
//
// When the platform granted a listener (manifest `sockets.listen`, delivered
// as an inherited fd), that listener is used — required on Linux, where this
// plugin runs in an empty network namespace and a self-bound 127.0.0.1 is a
// dead private loopback the extension could never reach. Falls back to
// self-binding when unsandboxed / on older platforms.
func (h *Host) startExtensionListener() (string, string, error) {
	// Inherited fd on Linux/macOS, the platform's relay on Windows — the
	// SDK knows the order. Taking InheritedListeners alone self-bound on
	// Windows, where a listener inside the AppContainer is unreachable.
	granted, err := branchkit.GrantedListeners()
	if err != nil {
		return "", "", fmt.Errorf("granted listener: %w", err)
	}
	var ln net.Listener
	if len(granted) > 0 {
		ln = granted[0]
		// This plugin declares exactly one socket; close any extras so we
		// don't leak fds.
		for _, extra := range granted[1:] {
			extra.Close()
		}
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", "", fmt.Errorf("bind: %w", err)
		}
	}

	token, err := generateToken()
	if err != nil {
		ln.Close()
		return "", "", err
	}

	server := newExtensionServer(h.extensionMux(token))
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			branchkit.Logf("browser", "extension listener error: %v", err)
		}
	}()

	return ln.Addr().String(), token, nil
}

// extensionMux is every route the extension talks to, built from the wire
// table (routes.go), each behind the pairing token (the event stream behind a
// one-time ticket minted with it).
func (h *Host) extensionMux(token string) *http.ServeMux {
	return h.buildExtensionMux(token)
}

// ticketHandler answers POST /events/ticket. EventSource cannot send headers,
// so the stream used to accept the pairing token in its URL — where it lands
// in history, logs and any proxy. Now the extension trades the token (header)
// for a one-time ticket that opens one stream within a short window; the
// token itself never travels in a URL.
//
// The request carries the extension's wire version. A mismatch is refused:
// the two sides were generated from different protocol declarations, and the
// strict decoders would otherwise fail request by request, invisibly. The
// refusal names this side's version so the extension can say which is out of
// date, and is logged at warn so it reaches branchkit.log.
func (h *Host) ticketHandler(tickets *ticketStore) func(*http.Request, *wire.TicketRequest) (wire.TicketResponse, error) {
	return func(_ *http.Request, req *wire.TicketRequest) (wire.TicketResponse, error) {
		ours := wire.Version()
		if req.WireVersion != ours {
			h.plugin.LogAt("warn", "WIRE_VERSION_MISMATCH", map[string]any{
				"msg":       "extension refused: built against a different extension wire; update the extension and BranchKit together",
				"extension": req.WireVersion,
				"plugin":    ours,
			})
			return wire.TicketResponse{}, &ticketRefusal{ours: ours}
		}
		t, err := tickets.mint()
		if err != nil {
			return wire.TicketResponse{}, err
		}
		return wire.TicketResponse{OK: true, Ticket: t, WireVersion: ours}, nil
	}
}

// ticketRefusal is the version-mismatch reply: a 409 whose body is the full
// TicketResponse (the extension reads wire_version off it).
type ticketRefusal struct{ ours string }

func (e *ticketRefusal) Error() string { return "wire_version_mismatch" }

func (e *ticketRefusal) reply() (int, any) {
	return http.StatusConflict, wire.TicketResponse{OK: false, Error: e.Error(), WireVersion: e.ours}
}

// eventsHandler answers GET /events: redeem the ticket, then hold the stream.
func (h *Host) eventsHandler(tickets *ticketStore) func(http.ResponseWriter, *http.Request, *wire.EventsQuery) {
	return func(w http.ResponseWriter, r *http.Request, q *wire.EventsQuery) {
		if !tickets.redeem(q.Ticket) {
			branchkit.Logf("browser", "[STREAM] refused: no valid ticket (conn_id %q)", q.ConnID)
			writeError(w, statusError(http.StatusUnauthorized, "unauthorized"))
			return
		}
		// Every connection-scoped map is keyed by conn_id; a stream without
		// one could never be addressed or drained.
		if q.ConnID == "" {
			writeError(w, badRequest("missing conn_id"))
			return
		}
		h.handleSSE(w, r, q.ConnID)
	}
}

// handleHealthGet carries the live stream-client count: the one positive
// signal that an extension holds an open event stream (a token-authed POST
// proves pairing, not the stream).
//
// Token-authed like every other route, and served without CORS headers
// (corsMiddleware). It used to answer anyone, with
// Access-Control-Allow-Origin: *, so any web page — or a DNS-rebinding
// attack — could read it and learn that this person runs BranchKit, an
// assistive tool. The extension never calls it; a person or agent debugging
// reads the token from connect.json.
func (h *Host) handleHealthGet(_ *http.Request, _ *wire.Empty) (wire.HealthResponse, error) {
	h.mu.Lock()
	clients := len(h.state.SSEClients)
	h.mu.Unlock()
	return wire.HealthResponse{Ready: true, StreamClients: clients}, nil
}

// handleSurgeryResultPost delivers the correlated reply for the tab-surgery
// request/response pattern (query_windows / move_active_tab_to_window /
// pop_active_tab / resize_window). request_id routes it to the goroutine
// parked in sendSurgeryAndWait; late replies are dropped. See tab_desk.go.
func (h *Host) handleSurgeryResultPost(_ *http.Request, reply *wire.SurgeryResult) (wire.Ack, error) {
	if reply.RequestID == "" {
		return wire.Ack{}, badRequest("missing request_id")
	}
	h.deliverSurgeryReply(*reply)
	return ack()
}

// Limits on the extension listener. It only ever talks to the extension on
// this machine, so none of these bind a real client; they bound what a stuck
// or hostile one can hold. No WriteTimeout or server-wide ReadTimeout: the
// event stream is one response that lives as long as the connection, and a
// server-wide read deadline also ends it (the connection's background read
// times out and cancels the request). Per-request deadlines are set in
// requestLimits instead, skipping the stream.
const (
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 2 * time.Minute
	serverMaxHeaderBytes    = 64 << 10
	requestReadTimeout      = 30 * time.Second
	requestWriteTimeout     = 30 * time.Second
	// Bodies: grammar batches, tab lists and command catalogs are kilobytes;
	// a palette with every bookmark can reach a megabyte or two.
	maxRequestBody = 8 << 20
	// The debug captures carry DOM dumps and a base64 viewport PNG.
	maxDebugRequestBody = 64 << 20
)

// eventStreamPath is the one long-lived response: no per-request deadlines.
var eventStreamPath = wire.EventsPath

// newExtensionServer wraps the route mux in the listener's middleware —
// outermost first: the Host check, per-request limits, CORS — and sets the
// server's own timeouts.
func newExtensionServer(mux http.Handler) *http.Server {
	return &http.Server{
		Handler:           loopbackHostOnly(requestLimits(corsMiddleware(mux))),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaxHeaderBytes,
	}
}

// loopbackHostOnly refuses a request whose Host header does not name this
// machine. The extension always addresses http://127.0.0.1:<port>; a page
// that re-points its own domain at 127.0.0.1 (DNS rebinding) reaches the
// same socket, but its requests carry ITS domain in Host — refused here
// before any route sees them.
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether a Host header (with or without a port) names
// the loopback interface: localhost, or a loopback IP literal.
func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requestLimits caps each request's body and, for everything but the event
// stream, how long reading and answering it may take.
func requestLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxRequestBody)
		if wire.LargeBodyPath(r.URL.Path) {
			limit = maxDebugRequestBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		// Deadlines are per CONNECTION and a keep-alive connection serves
		// request after request — the server resets neither between them
		// when its own timeouts are off. So every request sets both: the
		// stream clears them (a pooled connection that served a POST first
		// would otherwise end the stream at that POST's deadline), every
		// other request sets fresh ones. Not supported by every
		// ResponseWriter (tests' recorders); the body cap still holds.
		rc := http.NewResponseController(w)
		if r.URL.Path == eventStreamPath {
			_ = rc.SetReadDeadline(time.Time{})
			_ = rc.SetWriteDeadline(time.Time{})
		} else {
			_ = rc.SetReadDeadline(time.Now().Add(requestReadTimeout))
			_ = rc.SetWriteDeadline(time.Now().Add(requestWriteTimeout))
		}
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware answers cross-origin preflights for the extension's routes.
// /health gets no CORS headers at all: nothing but a local, token-holding
// caller has any business reading it.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			// PUT is not a CORS-safelisted method: a preflight that does not
			// name it refuses PUT /session for any caller that is subject to
			// CORS (an extension with host permission is not, but the answer
			// should not depend on that).
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validateToken checks the Authorization Bearer header.
func validateToken(r *http.Request, expected string) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// ticketStore mints one-time, short-lived stream tickets. A ticket is the
// only credential /events accepts: the extension obtains it with the
// pairing token in a header, so the token never appears in a URL.
type ticketStore struct {
	mu      sync.Mutex
	pending map[string]time.Time
}

const ticketTTL = 30 * time.Second

func newTicketStore() *ticketStore {
	return &ticketStore{pending: map[string]time.Time{}}
}

func (s *ticketStore) mint() (string, error) {
	t, err := generateToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.pending {
		if now.After(exp) {
			delete(s.pending, k)
		}
	}
	s.pending[t] = now.Add(ticketTTL)
	return t, nil
}

// redeem consumes a ticket: true once, within its window, never again.
func (s *ticketStore) redeem(t string) bool {
	if t == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.pending[t]
	if !ok {
		return false
	}
	delete(s.pending, t)
	return time.Now().Before(exp)
}

// handleDebugLogPost relays a diagnostic event from the extension into
// branchkit.log via stderr. Body shape: {"tag": "BK_HINT_BADGE", "data": {...}}.
// The tag is prefixed verbatim so log lines stay greppable.
func (h *Host) handleDebugLogPost(_ *http.Request, req *wire.DebugLogRequest) (wire.Ack, error) {
	if req.Tag == "" {
		return wire.Ack{}, badRequest("missing tag")
	}
	branchkit.Logf(h.state.PluginID, "[%s] %s", req.Tag, string(req.Data))
	return ack()
}

// handlePluginDebugLogPost forwards a diagnostic event from the extension
// to the per-plugin debug log channel. The platform writes the entry to
// plugin-logs/<pluginID>.log instead of the shared branchkit.log. Body shape:
// {"tag": "BK_ACTIVATE_PATH", "data": {...}, "level": "info"}. Empty tag
// is allowed (the SDK call will elide the [TAG] column in the rendered
// line). Optional `level` field defaults to "debug"; unknown levels also
// fall through to "debug" via plugin.LogAt.
func (h *Host) handlePluginDebugLogPost(_ *http.Request, req *wire.PluginDebugLogRequest) (wire.Ack, error) {
	level := req.Level
	if level == "" {
		level = "debug"
	}
	h.plugin.LogAt(level, req.Tag, req.Data)
	return ack()
}

// handleReferencesPost receives reference names from the extension and updates
// the voice grammar so "reference <name>" matches saved references.
func (h *Host) handleReferencesPost(_ *http.Request, req *wire.ReferenceNamesRequest) (wire.Ack, error) {
	h.mu.Lock()
	pluginID := h.state.PluginID
	h.mu.Unlock()

	if err := h.updateReferenceNames(pluginID, req.Names); err != nil {
		branchkit.Logf(pluginID, "reference names push failed: %v", err)
		return wire.Ack{}, statusError(http.StatusBadGateway, "reference push failed")
	}

	branchkit.Logf(pluginID, "reference names updated: %d names", len(req.Names))
	return ack()
}

// handleReferencesGet returns stored references for a host from the platform
// collection. Used by the extension for hydrate-on-connect (reinstall recovery).
func (h *Host) handleReferencesGet(_ *http.Request, q *wire.ReferencesQuery) (wire.ReferencesResponse, error) {
	host := q.Host
	if host == "" {
		return wire.ReferencesResponse{}, badRequest("missing host parameter")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	h.mu.Unlock()

	refs, err := h.loadReferencesForHost(pluginID, host)
	if err != nil {
		branchkit.Logf(pluginID, "reference load failed for %s: %v", host, err)
		refs = nil
	}
	if refs == nil {
		refs = make(map[string]any)
	}

	return wire.ReferencesResponse{References: refs}, nil
}

// handleReferenceSavePost saves a reference to the platform's durable collection.
func (h *Host) handleReferenceSavePost(_ *http.Request, req *wire.ReferenceSaveRequest) (wire.Ack, error) {
	if req.Host == "" || req.Name == "" {
		return wire.Ack{}, badRequest("missing host or name")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	h.mu.Unlock()

	if err := h.saveReferenceToCollection(pluginID, req.Host, req.Name, req.Reference); err != nil {
		branchkit.Logf(pluginID, "reference save to collection failed: %v", err)
		return wire.Ack{}, statusError(http.StatusBadGateway, "collection save failed")
	}

	branchkit.Logf(pluginID, "reference saved to collection: %s/%s", req.Host, req.Name)
	return ack()
}

// DispatchResultRequest is the body of POST /dispatch-result (wire.DispatchResult).
type DispatchResultRequest = wire.DispatchResult

// handleDispatchResultPost logs the outcome of one extension-side action
// attempt. Keeps branchkit.log as the single place to trace voice → element
// activation end-to-end.
func (h *Host) handleDispatchResultPost(_ *http.Request, reqp *DispatchResultRequest) (wire.Ack, error) {
	req := *reqp
	if req.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	tabID := h.state.FocusedTabID
	h.mu.Unlock()

	detail := ""
	if req.Detail != "" {
		detail = " detail=" + req.Detail
	}
	followField := ""
	if req.Followup != "" {
		followField = " followup=" + req.Followup
	}
	followField += " conn=" + req.ConnID
	if req.Unseen != "" {
		followField += " unseen=" + req.Unseen
	}
	branchkit.Logf(pluginID, "dispatch-result tab=%d action=%s codeword=%q resolved=%s elem_tag=%s taken=%s ok=%v frame=%s%s%s",
		tabID, req.Action, req.Codeword, req.Resolution, req.ElemTag, req.Taken, req.OK, req.Frame, detail, followField)

	h.handleDispatchFollowup(pluginID, req.Followup, req.ConnID)

	// Pull-resolution "no such hint": a sealed-alphabet pair with no live target — a mishear, or a
	// stale/off-screen pair the dispatch-time strict gate refused. Warn level
	// so it cross-posts to branchkit.log next to the COMMAND.MATCHED it
	// explains.
	if req.Detail == "no_such_hint" {
		h.plugin.LogAt("warn", "HINTS", map[string]any{
			"msg":      "no such hint — sealed-alphabet pair had no live target",
			"codeword": req.Codeword,
			"action":   req.Action,
			"unseen":   req.Unseen,
		})
	}
	// The extension refused the command (problem.go refusalFor): tell the
	// person, as for a command that reached no browser.
	if refusal, ok := h.refusalFor(req); ok {
		// Exit-side twin of visible ⇒ speakable: a refused hint activation is a
		// pure non-event, so it must NOT consume the hint mode. `activate`'s
		// manifest ClearsTags dropped plugin.browser.hints at MATCH time (before
		// dispatch); the click never landed, so roll that back. This is keyed on
		// the dispatch RESULT, not the match — the one signal that distinguishes
		// a real click from a mishear onto a dead pair. Level-derived through the
		// single policy point (re-arms iff the browser is still focused and the
		// focused tab still carries grammar — i.e. badges still on screen), so a
		// focus change since dispatch correctly idles it. hints is an as_gates
		// Layer-2 tag, so this is a free gate flip, never a grammar recompose.
		// Only for refusals that left the badges up (refusalFor says which).
		// applyDerivedHintsTag takes mu itself; must be called unlocked.
		if refusal.rearm {
			h.applyDerivedHintsTag(refusal.problem.Reason + "_re_arm")
		}
		h.reportActionProblem(req.Action, refusal.problem)
	}

	h.emitEvent("browser.dispatch_result", map[string]any{
		"tab_id":     tabID,
		"action":     req.Action,
		"codeword":   req.Codeword,
		"resolution": req.Resolution,
		"elem_tag":   req.ElemTag,
		"taken":      req.Taken,
		"ok":         req.OK,
		"frame":      req.Frame,
		"detail":     req.Detail,
		"followup":   req.Followup,
		"conn_id":    req.ConnID,
		"unseen":     req.Unseen,
	})
	return ack()
}

// handleHintsSessionEndPost clears `plugin.browser.hints` so the matcher
// stops gating on hints, and Deletes the per-frame (or per-tab)
// session's tracked codewords so disconnected-tab badges don't keep
// matching from the per-prefix collections. Fired by the extension on
// tab switch (whole-tab cleanup) or on frame detach (frame-scoped
// cleanup via the frame_id field).
//
// FrameID is a *int so we can distinguish "frame 0" (a real frame) from
// "frame_id omitted" (whole-tab cleanup). Chrome's top-frame id is 0.
func (h *Host) handleHintsSessionEndPost(_ *http.Request, req *wire.HintsSessionEndRequest) (wire.Ack, error) {
	if req.ConnID == "" {
		// Session keys are conn-scoped (tab ids are browser-local); an
		// unscoped end can't name a session and must not guess.
		return wire.Ack{}, badRequest("missing conn_id")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	var deleted int
	if req.FrameID != nil {
		key := frameKey{ConnID: req.ConnID, TabID: req.TabID, FrameID: *req.FrameID}
		if session, ok := h.state.FrameSessions[key]; ok {
			// Doc-identity fence (the document-scoped pool's grammar
			// sibling): a frame-scoped end names the DOCUMENT it is about
			// and may only tear down that document's session. A
			// navigation's liveness disconnect can be delivered seconds
			// late (4.5s observed on Firefox), landing after the successor
			// document's batches have repopulated this same (conn, tab,
			// frame) key — an unfenced cleanup then destroys hundreds of
			// live codewords while the successor's delta-sync shadow still
			// believes them committed: painted badges, voice-dead until an
			// unrelated republish (the 2026-07-24 wikipedia ZY repro).
			// Empty session.DocID (no batch seen yet) can't discriminate
			// and stays tearable — a session with no batches has nothing
			// to lose.
			if session.DocID != "" && session.DocID != req.DocID {
				branchkit.Logf(pluginID,
					"session_end ignored (doc mismatch) tab=%d frame=%d reason=%s have_doc=%.8s req_doc=%.8s",
					req.TabID, *req.FrameID, req.Reason, session.DocID, req.DocID)
			} else {
				deleted = h.cleanupFrameSessionLocked(pluginID, key, session, "session_end_frame_"+req.Reason)
				delete(h.state.FrameSessions, key)
			}
		}
	} else {
		// Tab-level session_end is only sent for genuine endings (tab
		// close/purge) — the SW's
		// per-switch tab_switch/window_focus ends are retired, and the
		// deproject/reproject now runs plugin-side from the focus recompute.
		deleted = h.cleanupTabSessionsLocked(pluginID, req.ConnID, req.TabID, "session_end_tab_"+req.Reason)
	}
	h.mu.Unlock()

	// A whole-tab session_end re-derives the hints tag from the focused
	// projection rather than dropping it: the ended tab may belong to a
	// background browser or be a background tab (closing it must not kill
	// hints in the tab the user is looking at), and when it WAS the focused
	// tab its sessions are gone now, so the derivation idles. A single frame
	// going away doesn't end the user's hint session — siblings in the same
	// tab may still be live.
	if req.FrameID == nil {
		h.applyDerivedHintsTag(fmt.Sprintf("session_end_%s conn=%s tab=%d deleted=%d",
			req.Reason, req.ConnID, req.TabID, deleted))
	}
	return ack()
}

// handleHintsSessionStartPost ensures the static hint-command skeleton is
// registered on tab activation, so the new tab's first grammar batch lands
// into a ready command surface. The tag itself arms from that batch's commit
// (transitionToGrammarOwned); there is no eager arm here any more.
func (h *Host) handleHintsSessionStartPost(_ *http.Request, _ *wire.Empty) (wire.Ack, error) {
	h.ensureSkeleton(h.state.PluginID)
	return ack()
}
