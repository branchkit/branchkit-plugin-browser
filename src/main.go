package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

var ()

// PluginState holds the runtime state for the browser plugin.
type PluginState struct {
	PluginID string

	// Whether the most recently focused app is a known browser. Tracked
	// from `_platform.app.focused` and gates background actions that
	// would otherwise leak browser-only state into other apps — most
	// notably, the per-batch grammar handler's hints-tag re-arm, which
	// fires whenever the CS pushes a batch (and the CS keeps pushing
	// after the user switches apps).
	BrowserFocused bool

	// Bundle ID of the OS-focused app (any app, browser or not), tracked
	// from `_platform.app.focused`. The per-prefix hint collections are a
	// global namespace shared by every connected browser, so this is the
	// authority for which browser's grammar batches are accepted: only the
	// OS-focused browser may write the live grammar (handleGrammarFramePut).
	// Background browsers keep scanning (their content scripts can't see app
	// focus) but their batches are rejected so they don't clobber what the
	// user actually sees. "" until the first focus event — gate fails open
	// then so the first batch after launch isn't dropped.
	FocusedBundleID string

	// The app the OS last REPORTED in front (app.focused, the boot prime) —
	// unlike FocusedBundleID, never set by an extension focus claim. A claim
	// that contradicts it gets a delayed OS re-check (ClaimVerifyTimer,
	// verifyFocusClaim), since a claim can arrive late.
	OSFrontmostBundle string
	ClaimVerifyTimer  *time.Timer

	// Bundle ID of the most recently OS-focused *browser*, sticky across
	// intervening non-browser focus. Names the browser being armed or
	// operated on (focus-gain arms, tab surgery). The switch that parks the
	// previous browser's grammar is decided per CONNECTION instead
	// (LastFocusedConnID), since two profiles share one bundle.
	// Distinct from FocusedBundleID, which tracks every app and so would
	// lose the prior browser across a Chrome -> Terminal -> Firefox hop.
	FocusedBrowserBundleID string

	// conn_id of the focused browser's connection: the live conn whose
	// AssertedBundle equals the OS-frontmost bundle, derived by
	// recomputeFocusedSourceLocked. Together with FocusedTabID this is the
	// authoritative "focused source" the per-source grammar projection keys off:
	// projectionSourceLocked builds the canonical collections from
	// this source only, and routing logs its tab. Empty when the frontmost app is
	// not a connected browser.
	FocusedConnID string

	// conn_id of the most recently focused browser connection, sticky across
	// intervening non-browser focus — the conn-level twin of
	// FocusedBrowserBundleID. A focus change to a DIFFERENT connection parks
	// this one's grammar (recomputeFocusedSourceLocked). Conn-level rather
	// than bundle-level because two profiles of one browser share a bundle:
	// a profile switch changes the connection without changing the bundle,
	// and the losing profile's codewords must stop matching all the same.
	LastFocusedConnID string

	// Every extension connection's state, by conn_id (conn.go): its stream,
	// focus claim, active tab, focused window, asserted bundle, tab words and
	// mode session. Dropped whole on the connection's SSE disconnect.
	Conns map[string]*Conn

	// The counter Conn.FocusSeq stamps come from: bumped on every focus
	// claim, so a connection's stamp orders its claim by arrival. Picks the
	// focused connection among several asserting the SAME bundle — two
	// profiles of one browser — as the tie-break under Conn.ClaimAt.
	FocusSeq uint64

	// Active tab id within the focused browser, reported explicitly by the
	// extension (POST /active-tab) rather than inferred from the last batch.
	// Batches arrive from every tab, so "last batch's tab" is meaningless — this
	// explicit signal is the source of truth for which tab projects and which
	// tab a click routes to. Derived from the focused Conn's ActiveTab whenever
	// focus changes, so a browser-switch picks up the new browser's already-known
	// active tab without waiting for it to re-report. 0 until the focused conn's
	// active tab is known.
	FocusedTabID int

	// SSE clients waiting for action dispatches
	SSEClients map[string]chan ActionEvent

	// clientID -> that stream's alphabet channel (capacity 1, latest wins).
	// Separate from SSEClients because the alphabet is its own named SSE
	// event, not an action, and must not be lost to a full action buffer.
	SSEAlphabet map[string]chan []string

	// The hint_display value last broadcast as set_badge_mode, so the mirror's
	// OnChange — which fires after EVERY decode, including the refresh before
	// each settings-tab render — only fans out an actual change. Each
	// broadcast is a chrome.storage write in the extension, and Chrome allows
	// 120 a minute: opening the browser settings tab used to spend that on a
	// no-op, and the user's next real edit was lost (2026-09-16). "" = nothing
	// broadcast yet, so the first decode always goes out.
	LastBadgeMode string

	// clientID -> OS-asserted bundle ID of that SSE connection. Lets dispatch and
	// rescan fan-outs target only the OS-focused browser's connection, so a spoken
	// command never fires in a background browser that still has a stale badge for
	// the same codeword. Parallel to SSEClients; same keys. Set from Conn.AssertedBundle
	// at SSE-accept (resolveAssertedBundle), the OS-resolved identity of the
	// connection's peer process.
	SSEClientBundles map[string]string

	// Extension hint visibility mode ("always" or "manual")
	HintVisibility string

	// State machine for `plugin.<id>.hints`. Owns the tag's Put/Delete and
	// the idle ⇄ grammar_owned lifecycle. All transitions go through the
	// gate's methods — no other code should touch `plugin.<id>.hints`
	// directly. See hint_gate.go.
	HintGate *hintGate

	// True once the hint-command skeleton (`<prefix:hints_prefix> <suffix:...>`,
	// trigger, hide, cancel) has been registered via `commands.push`.
	// Set by `ensureSkeleton` on the first browser focus / tab-switch /
	// per-batch arrival so the per-prefix collections per-batch grammar
	// Puts have a command pattern to bind against. Cleared never (the
	// skeleton stays valid for the plugin's lifetime).
	HintCommandSkeletonRegistered bool

	// Debounce timer for plugin.VocabularyCommit. The per-batch handler
	// fires VocabularyCommit whenever the visible codeword set changes; on a
	// heavy scroll burst that can be 3 commits in 200ms, each driving a full
	// recognizer rebuild. Coalesce into one commit per 50ms quiet
	// window. Held under `mu`; nil when no commit is pending.
	VocabularyCommitTimer *time.Timer

	// Per-(conn, tab, frame) grammar: each frame's last PUT /grammar/frame,
	// which replaces the one before it. Allocated on a frame's first request.
	FrameSessions map[frameKey]*FrameSession

	// collection name -> fingerprint of the (id, payload) set last
	// successfully synced to it. Skips a replace that would change nothing;
	// see replaceHintCollection for why this is safe where the deleted
	// payload shadows were not — losing it costs one redundant idempotent
	// replace, where losing a shadow produced wrong deletions.
	//
	// Written only AFTER the wire call succeeds, so a failed sync re-sends.
	// Cleared wherever the platform's view can change without this plugin
	// sending it (disconnect reset, calibration fixture apply/release), since
	// a fingerprint is a claim about what the platform holds, and any other
	// writer to these collections invalidates the claim.
	LastSyncFingerprint map[string]uint64

	// collection name -> fingerprint of the ID set alone (the spoken keys)
	// last successfully synced to it, beside LastSyncFingerprint's (id,
	// payload) one. A replace that moves only payloads (an element re-id'd,
	// a letter re-lettered, a strict flag flipped) leaves the words the
	// recognizer can hear unchanged, so it must not cost a vocabulary
	// commit. Same lifetime and same claim as LastSyncFingerprint: reset
	// together, seeded together (seedHintSyncFingerprints).
	LastSyncKeyFingerprint map[string]uint64

	// Set when a hint-collection sync changed some collection's spoken-key
	// set; consumed (read and cleared, under `mu`) by the grammar-frame
	// handler, which schedules the debounced vocabulary.commit only then.
	// A change made by a path that does not commit (reproject, cleanup,
	// park) stays set and rides the next frame's commit.
	HintVocabularyDirty bool

	// Spoken keys currently projected into `browser_tabs`. Drives the
	// "did the key set gain words?" check that gates the debounced
	// vocabulary commit after a tabs sync (the byte-level record diff is
	// the platform's, computed inside collection.replace).
	LastPushedTabKeys map[string]bool

	// Command-palette voice session (palette.go). Single slot — palettes
	// only exist in the focused browser. Since the `@palette` surface
	// migration the plugin holds no tag bookkeeping: the platform's
	// `_surface.browser.palette` tag follows the rows collection's contents.
	PaletteConnID   string
	PaletteEntries  []PaletteEntry
	PaletteReserves []string
	// No live-prefix index here. Which `browser_palette_<prefix>` collections
	// hold records is DERIVED at sync time from collections.owned
	// (palette.go's livePalettePrefixes), not remembered — a remembered index
	// forgets what a previous process hydrated. Same deletion the hints family
	// already made (batch.go's livePrefixes).

	// Caret/visual selection voice mode (caret.go). Held while the extension's
	// caret mode is active (its session's caret, session.go). Single slot — caret only exists
	// in the focused browser. CaretTagSet mirrors whether the EXCLUSIVE
	// plugin.<id>.caret tag is held platform-side. Same imperative, content-
	// driven shape as the palette tag (no clear_on_unrelated_command — the tag
	// is a pure mirror of the extension's mode, so it can't drift), just gating
	// caret voice commands instead of palette rows.
	CaretConnID string
	CaretActive bool
	CaretTagSet bool
	// The connection the caret tag was last set for: who browser.caret_updated
	// names when the tag is released (CaretConnID is cleared before that).
	CaretTagConnID string
	// CaretTagUncertain: the last caret tag write timed out, so whether the
	// platform holds the tag is unknown. See syncCaretLocked.
	CaretTagUncertain bool

	// Video mode (video.go). Held while the extension's video key layer is
	// live — the mode stack's mirror carries it in the session on the layer's one
	// lifetime (entered by `w` OR the spoken "video", exited by Escape/q/w/
	// "over"). Same pure-mirror shape as caret since Wave 3 C4b; before that
	// the tag was matcher-written and hold-scoped, a different lifetime from
	// the sticky key layer (the mismatch caret.go's modeMirrors used to
	// document as the reason video stayed out of the forwarder table).
	VideoModeConnID string
	VideoModeActive bool
	VideoModeTagSet bool

	// Keep-scrolling mode (autoscroll.go). Held while the extension's
	// auto-scroll run is live — the same pure-mirror shape as video: the mode
	// stack's mirror carries it in the session for the run's one lifetime. Exclusive,
	// so every drain path matters (focus.go, sse.go).
	AutoScrollConnID string
	AutoScrollActive bool
	AutoScrollTagSet bool

	// Query-field gate (query_field.go): focus sits in a single-line text
	// input, so a dictation landing there is a query and the platform strips
	// its prosody punctuation. Same mirror shape as caret, but NON-exclusive —
	// it shapes dictation and gates nothing, so a stale one is cosmetic.
	QueryFieldConnID string
	QueryFieldActive bool
	QueryFieldTagSet bool

	// Range-pick chip window (range_pick.go). While armed, the "highlight"
	// disambiguation chips own the hint codewords in ONE (conn, tab), and
	// buildTabPrefixState projects only these — so the Discovery HUD shows the
	// chips instead of the whole page's hints. Not a tag: the narrow covers
	// hint codewords only, not the whole command set. Nil/empty = unarmed.
	RangePickConnID    string
	RangePickTabID     int
	RangePickCodewords map[string]bool

	// Find-session voice gating (find.go). Held while the extension's find
	// session is active (its session's find) — bar open OR committed
	// highlights. NON-exclusive (find-jump augments browsing) and NOT drained
	// on unfocus (the session survives blur; the contributed commands gate on
	// app-active AND this tag instead). Single slot, like caret.
	FindConnID string
	FindActive bool
	FindTagSet bool

	// Media-active gating (video.go). The assertions are per connection
	// (Conn.Session.MediaActive: "this browser has controllable media" —
	// focused-tab video OR audible tab OR resume memory, the union computed
	// extension-side). The NON-exclusive plugin.<id>.media_active tag mirrors
	// the OR; MediaTagSet mirrors whether it's held platform-side.
	// Deliberately survives browser unfocus (background control is the
	// point); an assertion goes with its connection.
	MediaTagSet bool
	// The connection whose media the user most recently started or was in
	// front of — the "what you hear" pick when a media verb has no focused
	// target (mediaTargetConnsLocked). Moved by a media_active false->true
	// edge and by focus on a browser with media (session.go), never by a
	// PUT that only repeats it. Stale values are harmless: the pick only
	// honors it while that connection still asserts.
	MediaLastConnID string

	// Bundles that have held a live extension connection at any point this
	// session. Unlike AssertedBundle (live conns only), entries persist after
	// disconnect — that persistence is what lets the stale-disconnect
	// tripwire tell "extension vanished" from "browser that never had one".
	// See disconnect_nudge.go.
	SeenBundles map[string]bool

	// One-nudge-per-episode latch for the stale-disconnect tripwire; cleared
	// when the bundle reconnects.
	DisconnectNudged map[string]bool

	// Pending grace timer for the tripwire (at most one — it tracks the
	// frontmost bundle) and the bundle it was armed for.
	DisconnectNudgeTimer  *time.Timer
	DisconnectNudgeBundle string

	// User config
}

// ActionEvent is pushed to the extension over SSE when a voice command matches.
type ActionEvent = wire.ActionEvent

func main() {
	pluginID := os.Getenv("BRANCHKIT_PLUGIN_ID")
	if pluginID == "" {
		pluginID = "browser"
	}

	h := newHost(branchkit.NewPlugin())

	// Mirror the voice-owned alphabet (codeword ↔ letter). Must be wired
	// before plugin.Run() so the on_ready fetch lands.
	h.initCodewordMirror()

	h.state = &PluginState{
		PluginID:               pluginID,
		SSEClients:             make(map[string]chan ActionEvent),
		SSEAlphabet:            make(map[string]chan []string),
		SSEClientBundles:       make(map[string]string),
		Conns:                  make(map[string]*Conn),
		HintGate:               newHintGate(h, pluginID),
		FrameSessions:          make(map[frameKey]*FrameSession),
		LastSyncFingerprint:    make(map[string]uint64),
		LastSyncKeyFingerprint: make(map[string]uint64),
		LastPushedTabKeys:      make(map[string]bool),
		// Restored from disk, not empty: a browser whose extension was
		// already wedged at launch is otherwise never "seen", so the
		// tripwire never arms and the worst case stays silent. See
		// seen_browsers.go.
		SeenBundles:      loadSeenBrowsers(),
		DisconnectNudged: make(map[string]bool),
	}

	// Settings mirror: OnChange re-broadcasts the badge mode to connected
	// extensions on every hint_display CHANGE (generic form, browser tab,
	// import). Not on every decode — see State.LastBadgeMode. It used to cache
	// the config on state.Config as well; nothing read that copy —
	// handleRenderSettings deliberately reads through the mirror instead, and
	// says so — so the cache was maintained for nobody.
	h.initConfig(h.plugin, func(c BrowserConfig) {
		h.mu.Lock()
		if c.HintDisplay == h.state.LastBadgeMode {
			h.mu.Unlock()
			return
		}
		h.state.LastBadgeMode = c.HintDisplay
		for _, ch := range h.state.SSEClients {
			select {
			case ch <- wire.NewAction("set_badge_mode", wire.SetBadgeModeParams{Mode: c.HintDisplay}):
			default:
			}
		}
		h.mu.Unlock()
	})

	// Subscribe to app focus events to track whether a known browser is active.
	h.plugin.OnAppFocused(h.handleAppFocused)

	// Reset extension hint visuals on session boundary (key release, cancel, continuous exit).
	h.plugin.OnInputSessionBoundary(h.handleSessionEnded)

	// Forward dependent-capture progress to the extension so it grays out
	// non-matching hints when the user speaks the prefix codeword. Replaces
	// the side-effect that the pre-Phase-A per-prefix noop commands carried.
	h.plugin.OnCaptureProgress(h.handleCaptureProgress)

	// When the platform force-clears a mode-mirror tag (voice "over" running
	// the global escape while caret/palette is open), forward the exit to the
	// extension so the visible mode follows the tag. See caret.go.
	h.reconcileExternalTagClears(pluginID)
	// Forward the platform's palette codeword assignments to the extension
	// (the `@palette` surface's write-back channel — palette.go).
	h.watchPaletteAssignments(pluginID)

	// No boot-time tag reconcile here anymore. The platform releases this
	// plugin's writer-held tags the moment the previous process terminates
	// (`held_by_writer` reaping in the platform's cleanup_terminated_plugin),
	// through the same delete path an explicit clear takes — so a fresh
	// process never inherits a stranded caret/palette/video_mode tag. The
	// hand-rolled sweep this replaced lived in boot_reconcile.go, one of four
	// copies of the same mechanism the platform now owns.

	// Start TCP listener for extension connections
	listenAddr, pairingToken, err := h.startExtensionListener()
	if err != nil {
		branchkit.Logf(pluginID, "listen error: %v", err)
		os.Exit(1)
	}
	branchkit.Logf(pluginID, "extension listener on %s", listenAddr)

	// Write connect.json for discovery
	pluginDir := branchkit.PluginDir()
	if pluginDir != "." {
		if err := writeDiscovery(pluginDir, listenAddr, pairingToken); err != nil {
			branchkit.Logf(pluginID, "warning: failed to write discovery: %v", err)
		}
	}

	// Browser uses dynamic action dispatch (every browser.* action forwards to
	// SSE clients), so we register a single on_action handler instead of using
	// HandleAction. This is the documented escape hatch for plugins that don't
	// know their action set at compile time.
	branchkit.HandleTyped(h.plugin, "on_action", h.handleOnAction)
	h.plugin.SettingsCSS(browserCSS)
	h.plugin.SettingsTab("hints", h.renderBrowserSettings)
	h.plugin.SettingsTab("backup", h.renderBackupSettings)
	branchkit.HandleCommand(h.plugin, "set_hint_display", h.handleSetHintDisplay)
	branchkit.HandleCommand(h.plugin, "download_settings_backup", h.handleDownloadSettingsBackup)

	branchkit.HandleTyped(h.plugin, branchkit.HookTrialApplyFixture, h.handleTrialApplyFixture)
	branchkit.HandleTyped(h.plugin, branchkit.HookTrialReleaseFixture, h.handleTrialReleaseFixture)

	// Push every command group at startup — scroll/find (contributed),
	// escape, tab-desk, references, and the hint skeleton if the alphabet is
	// already loaded (it usually is by the time we reach here, since the
	// voice plugin pushes it during its own startup). Groups that are still
	// empty are pushed as empty, so the whole set is determined in one pass
	// instead of settling as each source first changes. After this, each
	// source re-pushes only its own group.
	//
	// Pre-2026-05-27 this was three fire-and-forget goroutines each doing an
	// independent commands.push, which had REPLACE-per-plugin semantics — so
	// the last-landing push won and scroll/find/refs/hints silently
	// overwrote each other. See commands_register.go.
	go func() {
		if err := h.pushAllCommandGroups(pluginID); err != nil {
			branchkit.Logf(pluginID, "startup command push: %v", err)
		}
	}()

	// Prime the cross-browser focus gate from the current frontmost app so
	// it isn't stuck failing open until the user's next app switch. Runs in
	// its own goroutine because plugin.Call needs the message loop (started
	// by plugin.Run below) to service the RPC response.
	go h.primeFocusedBundle(pluginID)

	// Reproject when frame sessions cross the stale TTL, so dormant-iframe
	// ghosts leave the grammar without waiting for an incidental batch.
	go h.sweepStaleFrameSessions()

	// Learn what the platform already holds in the hint collections, so a
	// plugin restart under a live platform does not re-send every one of
	// them. Needs the message loop for its reads, like the
	// prime above.
	go h.seedHintSyncFingerprints(pluginID)

	// Run the message loop (blocks until stdin closes or SIGTERM)
	h.plugin.Run()

	// Cleanup discovery file on shutdown
	if pluginDir != "." {
		os.Remove(filepath.Join(pluginDir, "connect.json"))
	}
}

func writeDiscovery(pluginDir, addr, token string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	info := map[string]string{
		"port":  port,
		"token": token,
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	path := filepath.Join(pluginDir, "connect.json")
	return os.WriteFile(path, data, 0600)
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
