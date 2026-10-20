package wire

// Auth is how a route authenticates its caller.
type Auth int

const (
	// AuthToken: the pairing token as a Bearer header.
	AuthToken Auth = iota
	// AuthTicket: a one-time ticket (the event stream; EventSource cannot
	// send headers). The handler redeems it.
	AuthTicket
)

// Route is one HTTP route of the extension listener. Req is the zero value of
// the request type (the JSON body, or for a GET the query parameters, each a
// string field). Resp is the zero value of the reply; nil for the event
// stream, whose events are StreamEvents.
type Route struct {
	Method string
	Path   string
	Req    any
	Resp   any
	Auth   Auth
	// Registered on dev builds only; not part of the extension's surface.
	Dev bool
	// Allowed the larger body limit (debug captures).
	LargeBody bool
	// One line for gen.ts.
	Doc string
}

// Key is the route's mux pattern, "METHOD /path".
func (r Route) Key() string { return r.Method + " " + r.Path }

// Stream reports whether this is the event stream.
func (r Route) Stream() bool { return r.Resp == nil }

// The routes. Each is a var so the plugin binds its handler by name and the
// compiler, not a string, ties the two.
var (
	GrammarFrame = Route{Method: "PUT", Path: "/grammar/frame", Req: GrammarFrameRequest{}, Resp: GrammarFrameResponse{},
		Doc: "A frame's whole grammar; replaces the last one for that frame."}
	HintsSessionEnd = Route{Method: "POST", Path: "/hints/session_end", Req: HintsSessionEndRequest{}, Resp: Ack{},
		Doc: "A tab's or a frame's hint session ended."}
	HintsSessionStart = Route{Method: "POST", Path: "/hints/session_start", Req: Empty{}, Resp: Ack{},
		Doc: "A hints-eligible tab activated: make sure the hint-command skeleton is registered."}
	RangePick = Route{Method: "POST", Path: "/range-pick", Req: RangePickRequest{}, Resp: Ack{},
		Doc: "Arm or release the disambiguation chip window."}
	DispatchResultRoute = Route{Method: "POST", Path: "/dispatch-result", Req: DispatchResult{}, Resp: Ack{},
		Doc: "The outcome of one dispatched action."}
	SurgeryResultRoute = Route{Method: "POST", Path: "/surgery/result", Req: SurgeryResult{}, Resp: Ack{},
		Doc: "The reply to one tab-surgery request."}

	Session = Route{Method: "PUT", Path: "/session", Req: SessionRequest{}, Resp: Ack{},
		Doc: "This connection's whole mode state (caret, find, video, keep-scrolling, query field, media); replaces the last."}
	Focus = Route{Method: "POST", Path: "/focus", Req: FocusRequest{}, Resp: Ack{},
		Doc: "A focus claim."}
	ActiveTab = Route{Method: "POST", Path: "/active-tab", Req: ActiveTabRequest{}, Resp: Ack{},
		Doc: "The active tab of this browser's window."}

	Tabs = Route{Method: "POST", Path: "/tabs", Req: TabsRequest{}, Resp: Ack{},
		Doc: "This connection's open-tab words."}
	Palette = Route{Method: "POST", Path: "/palette", Req: PaletteRequest{}, Resp: Ack{},
		Doc: "Open or close the palette's voice session."}

	CommandsContribute = Route{Method: "POST", Path: "/commands/contribute", Req: ContributeRequest{}, Resp: Ack{},
		Doc: "Replace this connection's contributed command catalog."}
	CommandOverride = Route{Method: "POST", Path: "/commands/override", Req: PhraseEdit{}, Resp: Ack{},
		Doc: "Set a command-phrase override (refusal text is the platform's validation)."}
	CommandOverrideReset = Route{Method: "POST", Path: "/commands/override/reset", Req: OverrideResetRequest{}, Resp: Ack{},
		Doc: "Revert a command-phrase override."}
	CommandOverrides = Route{Method: "GET", Path: "/commands/overrides", Req: Empty{}, Resp: OverridesResponse{},
		Doc: "This plugin's phrase overrides."}
	CommandAlias = Route{Method: "POST", Path: "/commands/alias", Req: PhraseEdit{}, Resp: Ack{},
		Doc: "Add a spoken form."}
	CommandAliasRemove = Route{Method: "POST", Path: "/commands/alias/remove", Req: PhraseEdit{}, Resp: Ack{},
		Doc: "Remove an added spoken form."}
	CommandAliases = Route{Method: "GET", Path: "/commands/aliases", Req: Empty{}, Resp: AliasesResponse{},
		Doc: "This plugin's added spoken forms."}
	HintDisplay = Route{Method: "POST", Path: "/settings/hint_display", Req: SetHintDisplayRequest{}, Resp: Ack{},
		Doc: "The popup's badge-label pick."}

	ReferenceNames = Route{Method: "POST", Path: "/references", Req: ReferenceNamesRequest{}, Resp: Ack{},
		Doc: "The saved reference names voice can match."}
	References = Route{Method: "GET", Path: "/references", Req: ReferencesQuery{}, Resp: ReferencesResponse{},
		Doc: "One host's stored references (reinstall recovery)."}
	ReferenceSave = Route{Method: "POST", Path: "/reference/save", Req: ReferenceSaveRequest{}, Resp: Ack{},
		Doc: "Save one reference."}

	SettingsBackupPut = Route{Method: "PUT", Path: "/settings-backup", Req: SettingsBackupPutRequest{}, Resp: Ack{},
		Doc: "Keep this browser's copy of the extension's settings (413 past 1 MB; 409 for an empty copy over a non-empty one without force)."}
	SettingsBackupGet = Route{Method: "GET", Path: "/settings-backup", Req: SettingsBackupQuery{}, Resp: SettingsBackupResponse{},
		Doc: "The kept copy of the extension's settings: this family's, else the newest."}

	DebugLog = Route{Method: "POST", Path: "/debug-log", Req: DebugLogRequest{}, Resp: Ack{},
		Doc: "A diagnostic line for branchkit.log."}
	PluginDebugLog = Route{Method: "POST", Path: "/plugin-debug-log", Req: PluginDebugLogRequest{}, Resp: Ack{},
		Doc: "A diagnostic line for this plugin's log."}
	PerfReport = Route{Method: "POST", Path: "/perf-report", Req: PerfReportRequest{}, Resp: Ack{},
		Doc: "A perf snapshot for the JSONL trail."}
	DebugSnapshotRoute = Route{Method: "POST", Path: "/debug-snapshot", Req: DebugSnapshot{}, Resp: Ack{}, LargeBody: true,
		Doc: "The structured half of a diagnostics snapshot."}
	DebugScreenshot = Route{Method: "POST", Path: "/debug-snapshot/screenshot", Req: DebugScreenshotRequest{}, Resp: Ack{}, LargeBody: true,
		Doc: "The snapshot's viewport PNG, or why there is none."}

	EventsTicket = Route{Method: "POST", Path: "/events/ticket", Req: TicketRequest{}, Resp: TicketResponse{},
		Doc: "A one-time ticket for the event stream; refused (409) on a wire_version mismatch."}
	Events = Route{Method: "GET", Path: "/events", Req: EventsQuery{}, Resp: nil, Auth: AuthTicket,
		Doc: "The event stream (SSE): connected, alphabet, action."}
	Health = Route{Method: "GET", Path: "/health", Req: Empty{}, Resp: HealthResponse{},
		Doc: "Liveness and stream-client count, for debugging (served without CORS headers)."}

	FocusPinGet = Route{Method: "GET", Path: "/dev/focus-pin", Req: Empty{}, Resp: FocusPinState{}, Dev: true,
		Doc: "Dev builds: the focus pin."}
	FocusPinSet = Route{Method: "POST", Path: "/dev/focus-pin", Req: FocusPinRequest{}, Resp: FocusPinState{}, Dev: true,
		Doc: "Dev builds: pin or unpin a connection as focused."}
)

// Routes is every route, in the order gen.ts lists them.
var Routes = []Route{
	GrammarFrame, HintsSessionEnd, HintsSessionStart, RangePick, DispatchResultRoute, SurgeryResultRoute,
	Session, Focus, ActiveTab,
	Tabs, Palette,
	CommandsContribute, CommandOverride, CommandOverrideReset, CommandOverrides, CommandAlias, CommandAliasRemove,
	CommandAliases, HintDisplay,
	ReferenceNames, References, ReferenceSave,
	SettingsBackupPut, SettingsBackupGet,
	DebugLog, PluginDebugLog, PerfReport, DebugSnapshotRoute, DebugScreenshot,
	EventsTicket, Events, Health,
	FocusPinGet, FocusPinSet,
}

// LargeBodyPath reports whether a path is allowed the larger body limit.
func LargeBodyPath(path string) bool {
	for _, r := range Routes {
		if r.LargeBody && r.Path == path {
			return true
		}
	}
	return false
}

// EventsPath is the one long-lived response: no per-request deadlines.
var EventsPath = Events.Path

// TSAlias is a named TypeScript type the generator emits for a closed
// vocabulary referenced by `ts` tags.
type TSAlias struct {
	Name string
	Type string
	Doc  string
}

// TSAliases are emitted before the interfaces.
var TSAliases = []TSAlias{
	{"HintVisibility", "'always' | 'manual'", "When hint badges show: always, or on demand."},
}
