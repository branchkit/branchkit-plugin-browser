package wire

import "encoding/json"

// The event stream (GET /events) carries three named SSE events. Their data
// is JSON: ConnectedEvent, AlphabetEvent and ActionEvent.

// ConnectedEvent opens every stream.
type ConnectedEvent struct {
	// The plugin's WIRE_VERSION, the same one the ticket checked.
	WireVersion string `json:"wire_version"`
}

// AlphabetEvent carries the 26 voice codewords, letter order.
type AlphabetEvent struct {
	Words []string `json:"words"`
}

// ActionEvent is one action for the extension. Params is a JSON object: the
// typed params of a synthetic action (SyntheticActions), or, for a
// contributed command, the params the platform matched — strings, except
// `targets`, the ordered list of a multi-target verb.
type ActionEvent struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params,omitempty" ts:"Record<string, unknown>"`
	// The inbound on_action's tr_ id; empty for synthetic events.
	CorrelationID string `json:"correlation_id,omitempty"`
}

// NewAction builds an ActionEvent with params marshaled once, here. A nil
// params is an event with none.
func NewAction(action string, params any) ActionEvent {
	ev := ActionEvent{Action: action}
	if params != nil {
		if b, err := json.Marshal(params); err == nil && string(b) != "null" {
			ev.Params = b
		}
	}
	return ev
}

// Param reads one param back as text (strings verbatim, anything else as its
// JSON). For logs and tests; the extension reads the typed shapes.
func (e ActionEvent) Param(key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(e.Params, &m) != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// --- Synthetic actions: minted by this plugin, not by a matched command ---

// ReactivateParams asks the extension to rebuild and re-push its grammar (a
// fresh stream, or a refocus that found the plugin's state for the tab gone).
type ReactivateParams struct {
	Reason string `json:"reason"`
	// "active" = the active tab only.
	Target string `json:"target,omitempty" ts:"'active'"`
}

// NoopParams un-narrows the display, or narrows it to a spoken prefix.
type NoopParams struct {
	Prefix string `json:"prefix,omitempty"`
}

// SetBadgeModeParams carries the settings collection's hint_display.
type SetBadgeModeParams struct {
	Mode string `json:"mode" ts:"'word' | 'letter' | 'expand'"`
}

// PaletteAssignParams carries the platform's codeword assignments for the
// open palette: row_id -> spoken codeword.
type PaletteAssignParams struct {
	Assignments map[string]string `json:"assignments"`
}

// ModeExitParams ends an extension mode whose tag something else cleared
// (select_exit, palette_dismiss, video_exit, auto_scroll_stop).
type ModeExitParams struct {
	Action string `json:"action"`
}

// QueryWindowsParams asks for this browser's normal windows with bounds.
type QueryWindowsParams struct {
	RequestID string `json:"request_id"`
}

// MoveActiveTabToWindowParams moves the active tab into a window.
type MoveActiveTabToWindowParams struct {
	RequestID string `json:"request_id"`
	WindowID  int    `json:"window_id"`
	Activate  bool   `json:"activate"`
}

// PopActiveTabParams moves the active tab into a new window, at these bounds
// when given.
type PopActiveTabParams struct {
	RequestID string `json:"request_id"`
	Left      *int   `json:"left,omitempty"`
	Top       *int   `json:"top,omitempty"`
	Width     *int   `json:"width,omitempty"`
	Height    *int   `json:"height,omitempty"`
}

// ResizeWindowParams sets a window's width (the reverse identity probe).
type ResizeWindowParams struct {
	RequestID string `json:"request_id"`
	WindowID  int    `json:"window_id"`
	Width     int    `json:"width"`
}

// SyntheticAction names one action this plugin mints and the shape of its
// params.
type SyntheticAction struct {
	Name   string
	Params any
}

// SyntheticActions is every action the plugin itself sends. Everything else
// on the stream is a contributed command's action.
var SyntheticActions = []SyntheticAction{
	{"reactivate", ReactivateParams{}},
	{"noop", NoopParams{}},
	{"set_badge_mode", SetBadgeModeParams{}},
	{"palette_assign", PaletteAssignParams{}},
	{"select_exit", ModeExitParams{}},
	{"palette_dismiss", ModeExitParams{}},
	{"video_exit", ModeExitParams{}},
	{"auto_scroll_stop", ModeExitParams{}},
	{"query_windows", QueryWindowsParams{}},
	{"move_active_tab_to_window", MoveActiveTabToWindowParams{}},
	{"pop_active_tab", PopActiveTabParams{}},
	{"resize_window", ResizeWindowParams{}},
}

// StreamEvent names one SSE event and its data.
type StreamEvent struct {
	Name string
	Data any
}

// StreamEvents is every named event on the stream.
var StreamEvents = []StreamEvent{
	{"connected", ConnectedEvent{}},
	{"alphabet", AlphabetEvent{}},
	{"action", ActionEvent{}},
}
