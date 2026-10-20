// Package wire is the one declaration of the HTTP + SSE protocol between the
// browser extension and this plugin: every route's request and reply, and
// every event the stream carries. The plugin's mux is built from Routes, so a
// route cannot exist untyped, and `go run ./cmd/wiregen` emits the
// extension's src/wire/gen.ts from these same types, so the two sides cannot
// drift without the drift gate (`just check-browser-wire`) seeing it.
//
// Field rules the generator follows: the json tag names the field; omitempty
// or a pointer makes it optional; a `ts:"..."` tag overrides the TypeScript
// type (string unions for the closed vocabularies). Doc comments on types and
// fields are carried into gen.ts.
package wire

import (
	"encoding/json"
	"time"
)

// Ack is the reply of every write route that returns no data: ok, or not ok
// with the reason. A refused request answers it with a 4xx/5xx status.
type Ack struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Empty is a request with no fields: a POST with an empty object, or a GET
// with no query parameters.
type Empty struct{}

// --- Grammar ---

// GrammarElement is one codeword a frame holds: only what the plugin consumes.
// Its frame is the request's; the element's label, category and adapter stay
// in the extension.
type GrammarElement struct {
	// Registry id minted by the content script; 0 = not addressable.
	ID int `json:"id"`
	// The scanner's element type ("link", "button", "record_action", ...).
	Type string `json:"type"`
	// The two-word spoken codeword ("arch bake").
	Codeword string `json:"codeword"`
	// The element intersects the strict viewport at send time: only these
	// enter the `_strict` companion collection (the Discovery HUD and the
	// activate command's capture). Band-only entries are painted, not matchable.
	InStrictViewport bool `json:"in_strict_viewport,omitempty"`
}

// GrammarFrameRequest is a frame's whole grammar: every codeword it holds right
// now. It replaces whatever the plugin held for that frame, so there are no
// deltas to lose, no ordering between requests to keep, and nothing to rebuild
// after either side restarts — the next request is complete by itself.
type GrammarFrameRequest struct {
	// Stamped by the service worker from the message sender.
	TabID int `json:"tab_id"`
	// Stamped by the service worker from the message sender.
	FrameID int `json:"frame_id"`
	// chrome.windows id of the sending tab, stamped by the service worker.
	// Tells the OS-focused window from a side-by-side window of the same
	// browser; 0 = unknown (the gate fails open on window).
	WindowID int `json:"window_id"`
	// The sending frame is foreground (document.visibilityState === 'visible').
	// A background tab reports false and is stored, never projected.
	Visible bool `json:"visible"`
	// The sending content script's document identity, stable for the
	// document's life. A frame-scoped session_end is fenced against it.
	DocID string `json:"doc_id"`
	// The extension's connection nonce, stamped by the service worker.
	ConnID         string           `json:"conn_id"`
	HintVisibility string           `json:"hint_visibility" ts:"HintVisibility"`
	Elements       []GrammarElement `json:"elements"`
}

// GrammarFailure says why one element was not stored.
type GrammarFailure struct {
	Codeword string `json:"codeword"`
	Reason   string `json:"reason"`
}

// GrammarFrameResponse acknowledges a frame's grammar.
type GrammarFrameResponse struct {
	// "ok" = stored and projected; "stored" = stored, not projected (the
	// source is not the focused one); "calibration_active" = refused during
	// a calibration trial (nothing stored; send again later).
	Result string           `json:"result" ts:"'ok' | 'stored' | 'calibration_active'"`
	Failed []GrammarFailure `json:"failed"`
	// The frame's codeword count after this request (a log breadcrumb).
	CommittedCodewords int `json:"committed_codewords"`
}

// HintsSessionEndRequest ends a hint session: a whole tab (frame_id absent) or
// one frame (frame_id and doc_id).
type HintsSessionEndRequest struct {
	ConnID string `json:"conn_id"`
	Reason string `json:"reason"`
	TabID  int    `json:"tab_id"`
	// Absent = the whole tab. A pointer because 0 is the top frame.
	FrameID *int `json:"frame_id,omitempty"`
	// The document this frame-scoped end is about; a session holding another
	// document's batches is left alone (a navigation's late disconnect).
	DocID string `json:"doc_id,omitempty"`
}

// RangePickRequest arms (codewords) or releases (empty) the disambiguation
// chip window: while armed the hint projection narrows to the chips.
type RangePickRequest struct {
	ConnID    string   `json:"conn_id"`
	TabID     int      `json:"tab_id"`
	Codewords []string `json:"codewords"`
}

// --- Dispatch outcomes ---

// DispatchResult is the outcome of one extension-side action attempt, posted
// after every dispatched action so the log traces voice to outcome.
type DispatchResult struct {
	// "activate", "scroll", "noop", ...
	Action string `json:"action"`
	// The codeword ("arch check"); empty for non-codeword actions;
	// resolve_reference carries the reference name.
	Codeword string `json:"codeword"`
	// How the target resolved. range_pick = a disambiguation chip,
	// search_badge = a search-match jump, extra_hint = something that only
	// looked clickable, reference = a saved reference.
	Resolution string `json:"resolution" ts:"'snapshot' | 'live_store' | 'range_pick' | 'search_badge' | 'extra_hint' | 'reference' | 'none'"`
	// Tag of the resolved element ("input", "a").
	ElemTag string `json:"elem_tag"`
	Taken   string `json:"taken" ts:"'focus' | 'click' | 'skipped' | 'noop'"`
	OK      bool   `json:"ok"`
	// The frame's location, trimmed.
	Frame string `json:"frame"`
	// An error or note; some values are refusal codes (problem.go refusalFor).
	Detail string `json:"detail"`
	// Work the page cannot finish and the platform can: open_select = press
	// the key that opens the native list of the <select> just focused.
	Followup string `json:"followup,omitempty" ts:"'open_select'"`
	// The posting connection: a follow-up acts only when it is in front.
	ConnID string `json:"conn_id"`
	// With detail "no_such_hint" only: the hint exists but cannot be seen.
	Unseen string `json:"unseen,omitempty" ts:"'off_screen' | 'not_visible' | 'disabled'"`
}

// WindowInfo is one browser window as the extension reports it for tab
// surgery.
type WindowInfo struct {
	WindowID       int    `json:"window_id"`
	Left           int    `json:"left"`
	Top            int    `json:"top"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	State          string `json:"state"`
	ActiveTabTitle string `json:"active_tab_title"`
	// The window holding the active tab — the tab any surgery would move.
	IsSource bool `json:"is_source"`
}

// SurgeryResult answers one tab-surgery request (query_windows,
// move_active_tab_to_window, pop_active_tab, resize_window), routed to the
// waiter by request_id.
type SurgeryResult struct {
	RequestID string `json:"request_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	// pop_active_tab: the window it created.
	Window *WindowInfo `json:"window,omitempty"`
	// query_windows: the full inventory.
	Windows []WindowInfo `json:"windows,omitempty"`
}

// --- Modes and focus ---

// SessionModes are the extension's mirrored modes, each a level: true while
// the mode is on in some live frame of this browser.
type SessionModes struct {
	// Caret/visual selection (exclusive caret tag).
	Caret bool `json:"caret"`
	// A find session: bar open or committed highlights (non-exclusive).
	Find bool `json:"find"`
	// The video key layer (exclusive video_mode tag).
	Video bool `json:"video"`
	// A keep-scrolling run (exclusive auto_scroll tag).
	AutoScroll bool `json:"auto_scroll"`
}

// SessionRequest is a connection's whole mode state. Every PUT replaces the
// last, so a dropped or reordered request is healed by the next one and a
// reconnect replays one request instead of an edge per mode. A mode turned on
// takes its tag only from the focused connection (or with focus unknown,
// except the query field); turned off, it releases the tag only if this
// connection holds it.
type SessionRequest struct {
	ConnID string       `json:"conn_id"`
	Modes  SessionModes `json:"modes"`
	// Focus is in a single-line text input: dictation there is a query.
	QueryField bool `json:"query_field"`
	// This browser has something media-controllable (focused-tab video,
	// an audible tab, or the resume memory).
	MediaActive bool `json:"media_active"`
}

// FocusRequest is a focus claim: a window of this connection's browser gained
// (or lost) OS focus.
type FocusRequest struct {
	ConnID  string `json:"conn_id"`
	Focused bool   `json:"focused"`
	// The OS-focused window; 0 when none.
	WindowID       int    `json:"window_id"`
	HintVisibility string `json:"hint_visibility" ts:"HintVisibility"`
	// Per connection, increasing in the order focus changed; a claim not newer
	// than the last applied one is dropped.
	Seq uint64 `json:"seq,omitempty"`
	// Epoch ms of the change; orders claims across connections.
	ClaimedAt int64 `json:"claimed_at,omitempty"`
}

// ActiveTabRequest reports the active tab of this browser's window. No focus
// claim; only the focused connection's report moves the focused tab.
type ActiveTabRequest struct {
	ConnID         string `json:"conn_id"`
	TabID          int    `json:"tab_id"`
	HintVisibility string `json:"hint_visibility" ts:"HintVisibility"`
}

// --- Spoken collections ---

// TabEntry is one spoken word for an open tab. TabID is a string because it
// becomes the value the matcher substitutes into action params.
type TabEntry struct {
	Spoken string `json:"spoken"`
	TabID  string `json:"tab_id"`
}

// TabsRequest publishes this connection's open-tab words.
type TabsRequest struct {
	ConnID  string     `json:"conn_id"`
	Entries []TabEntry `json:"entries"`
}

// PaletteEntry is one palette row. Spoken present = a pinned codeword (the
// tabs scope's marks); absent = the platform assigns. RowID is the
// extension's opaque handle; Title and Group are display-only.
type PaletteEntry struct {
	Spoken string `json:"spoken,omitempty"`
	Title  string `json:"title"`
	Group  string `json:"group"`
	RowID  string `json:"row_id"`
}

// PaletteRequest opens (entries) or closes (no entries) the palette's voice
// session.
type PaletteRequest struct {
	ConnID   string         `json:"conn_id"`
	Entries  []PaletteEntry `json:"entries"`
	Reserves []string       `json:"reserves,omitempty"`
}

// --- Commands ---

// ContributedCommand is one command of the extension's static vocabulary.
// Action is a bare id; the plugin prefixes it and owns the context gate.
type ContributedCommand struct {
	Action      string            `json:"action"`
	Pattern     string            `json:"pattern"`
	Params      map[string]string `json:"params,omitempty"`
	Category    string            `json:"category,omitempty"`
	Description string            `json:"description,omitempty"`
	// For {hint} patterns: the verb leaves hints painted and live, so the
	// plugin must not clear the hints tag.
	RetainsHints bool `json:"retains_hints,omitempty"`
	// Voice-context gate: "palette", "caret", "video", "auto_scroll", "find";
	// empty = app-active.
	Context string `json:"context,omitempty"`
	// Prefix-discovery affordance ("prefix" | "exclusive"), passed through to
	// the platform, which validates it.
	Discovery string `json:"discovery,omitempty"`
}

// ContributeRequest replaces this connection's contributed catalog.
type ContributeRequest struct {
	ConnID   string               `json:"conn_id"`
	Commands []ContributedCommand `json:"commands"`
}

// PhraseEdit is a command-phrase override or alias in the extension's
// notation: a bare action id and {..} captures.
type PhraseEdit struct {
	Action         string `json:"action"`
	DefaultPattern string `json:"default_pattern"`
	NewPattern     string `json:"new_pattern"`
}

// OverrideResetRequest reverts one override to the default phrase.
type OverrideResetRequest struct {
	Action         string `json:"action"`
	DefaultPattern string `json:"default_pattern"`
}

// OverridesResponse lists this plugin's phrase overrides.
type OverridesResponse struct {
	Overrides []PhraseEdit `json:"overrides"`
}

// AliasesResponse lists this plugin's added spoken forms.
type AliasesResponse struct {
	Aliases []PhraseEdit `json:"aliases"`
}

// SetHintDisplayRequest relays the popup's badge-label pick into the settings
// collection, the setting's one owner.
type SetHintDisplayRequest struct {
	HintDisplay string `json:"hint_display" ts:"'word' | 'letter' | 'expand'"`
}

// --- References ---

// ReferenceNamesRequest replaces the set of saved reference names voice can
// match.
type ReferenceNamesRequest struct {
	Names []string `json:"names"`
}

// ReferenceSaveRequest saves one reference to the durable collection.
type ReferenceSaveRequest struct {
	Host      string         `json:"host"`
	Name      string         `json:"name"`
	Reference map[string]any `json:"reference"`
}

// ReferencesQuery asks for one host's stored references.
type ReferencesQuery struct {
	Host string `json:"host"`
}

// ReferencesResponse is one host's stored references, by name.
type ReferencesResponse struct {
	References map[string]any `json:"references"`
}

// --- Settings backup ---

// SettingsBackupPutRequest stores this browser's copy of the extension's
// durable settings, replacing the copy kept for its browser family. The
// document is the extension's own backup file format; the plugin keeps it
// as given and reads only whether its settings are empty.
type SettingsBackupPutRequest struct {
	ConnID  string `json:"conn_id"`
	Browser string `json:"browser" ts:"'chrome' | 'firefox'"`
	// The backup document: {schemaVersion, exportedAt, extensionVersion,
	// browser, settings: {sync, local}}.
	Backup json.RawMessage `json:"backup"`
	// Replace a copy that has settings with one that has none. Without it
	// that write is refused (409): a fresh install must not overwrite the
	// good copy before the user has chosen to restore it or start fresh.
	Force bool `json:"force,omitempty"`
}

// SettingsBackupQuery asks for the kept copy of the extension's settings.
type SettingsBackupQuery struct {
	// Prefer this browser family's copy; without one, the newest of any.
	Browser string `json:"browser,omitempty" ts:"'chrome' | 'firefox'"`
}

// SettingsBackupResponse is the kept copy, or found:false when there is none.
type SettingsBackupResponse struct {
	Found bool `json:"found"`
	// The browser family the copy came from.
	Browser string `json:"browser,omitempty"`
	// When the plugin stored it, epoch ms.
	SavedAt int64 `json:"saved_at,omitempty"`
	// The document as the extension sent it.
	Backup json.RawMessage `json:"backup,omitempty"`
}

// --- Diagnostics ---

// DebugLogRequest relays a diagnostic line into branchkit.log.
type DebugLogRequest struct {
	// Required; prefixed verbatim so lines stay greppable.
	Tag  string          `json:"tag"`
	Data json.RawMessage `json:"data,omitempty"`
}

// PluginDebugLogRequest relays a diagnostic line into this plugin's own log.
type PluginDebugLogRequest struct {
	Tag  string          `json:"tag"`
	Data json.RawMessage `json:"data,omitempty"`
	// trace / debug / info / warn / error; empty or unknown = debug.
	Level string `json:"level,omitempty"`
}

// PerfReportRequest is one content-script perf snapshot, appended to a JSONL
// trail for offline analysis.
type PerfReportRequest struct {
	URL      string          `json:"url"`
	TabID    int             `json:"tab_id"`
	Browser  string          `json:"browser"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// DebugSnapshot is the structured half of a hint-diagnostics snapshot. Any
// JSON object; snapshot_id is required (and checked for filesystem safety).
type DebugSnapshot json.RawMessage

// TSType is the generator's override: an open object with one required key.
func (DebugSnapshot) TSType() string {
	return "{ snapshot_id: string; [key: string]: unknown }"
}

// UnmarshalJSON keeps the raw object (a named RawMessage type loses the
// method set it would need to decode itself).
func (d *DebugSnapshot) UnmarshalJSON(b []byte) error {
	*d = append((*d)[:0], b...)
	return nil
}

// DebugScreenshotRequest attaches the viewport PNG, or the capture error, to
// a snapshot. Exactly one of png_base64 / error.
type DebugScreenshotRequest struct {
	SnapshotID string `json:"snapshot_id"`
	PngBase64  string `json:"png_base64,omitempty"`
	Error      string `json:"error,omitempty"`
}

// --- Stream ---

// TicketRequest buys a one-time ticket for the event stream. The pairing
// token travels in the header; only the ticket goes in the stream URL.
type TicketRequest struct {
	// The extension's WIRE_VERSION. A mismatch is refused (409): the two
	// sides were built from different protocol declarations.
	WireVersion string `json:"wire_version"`
}

// TicketResponse carries the ticket, or the refusal. wire_version is always
// the plugin's, so a refused extension can say which side is out of date.
type TicketResponse struct {
	OK          bool   `json:"ok"`
	Ticket      string `json:"ticket,omitempty"`
	Error       string `json:"error,omitempty" ts:"'wire_version_mismatch' | string"`
	WireVersion string `json:"wire_version"`
}

// EventsQuery opens the event stream. EventSource cannot send headers, hence
// the query.
type EventsQuery struct {
	Ticket string `json:"ticket"`
	ConnID string `json:"conn_id"`
}

// HealthResponse is for a person or agent debugging: the extension never
// calls it.
type HealthResponse struct {
	Ready         bool `json:"ready"`
	StreamClients int  `json:"stream_clients"`
}

// --- Dev-build focus pin (not called by the extension) ---

// FocusPinRequest pins a live connection as the focused one; empty conn_id or
// off unpins.
type FocusPinRequest struct {
	ConnID string `json:"conn_id,omitempty"`
	Off    bool   `json:"off,omitempty"`
}

// FocusPinConn is one live connection.
type FocusPinConn struct {
	ConnID    string `json:"conn_id"`
	Bundle    string `json:"bundle"`
	ActiveTab int    `json:"active_tab"`
}

// PinnedEffect is one focus-scoped effect the pin suppressed.
type PinnedEffect struct {
	Kind       string         `json:"kind"`
	Action     string         `json:"action"`
	Params     map[string]any `json:"params,omitempty"`
	PinnedConn string         `json:"pinned_conn"`
	At         time.Time      `json:"at"`
}

// FocusPinState answers both focus-pin routes.
type FocusPinState struct {
	Pinned            bool           `json:"pinned"`
	ConnID            string         `json:"conn_id,omitempty"`
	Since             *time.Time     `json:"since,omitempty"`
	FocusedConnID     string         `json:"focused_conn"`
	BrowserFocused    bool           `json:"browser_focused"`
	OSFrontmostBundle string         `json:"os_frontmost_bundle"`
	Connections       []FocusPinConn `json:"connections"`
	Suppressed        []PinnedEffect `json:"suppressed"`
}
