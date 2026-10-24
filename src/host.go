package main

import (
	"sync"

	"github.com/branchkit/plugin-sdk-go"
)

// Host is what every handler in the browser plugin needs: the platform
// handle, the plugin state with the mutex that guards it, the settings
// mirror, the calibration fixture, and the smaller guarded groups that used
// to be package variables. Handlers are methods on it. The func-typed
// fields are the platform calls handler tests swap for stubs; each starts
// as its Default method.
type Host struct {
	plugin *branchkit.Plugin

	mu    sync.Mutex
	state *PluginState

	configMirror *branchkit.SettingsMirror[BrowserConfig]
	calibration  *calibrationFixture

	pushMu       sync.Mutex
	perfReportMu sync.Mutex

	// Per-connection contributed command catalogs.
	contribMu   sync.RWMutex
	contributed map[string]connCatalog
	contribSeq  uint64

	// The voice-owned alphabet, mirrored.
	codewords      [26]string
	codewordsMu    sync.RWMutex
	codewordMirror *branchkit.CollectionMirror

	// Reference names for the references collection.
	refNamesMu sync.RWMutex
	refNames   []string

	// Tab-desk surgery replies awaited by connection.
	surgeryMu      sync.Mutex
	surgeryWaiters map[string]chan surgeryReply

	// The HUD channel problems are told on (problem.go).
	problems problemSurface

	// The dev-build focus pin and the effects it suppressed (focus_pin.go).
	pin focusPin

	// The Backup tab's last download outcome per browser family
	// (settings_backup_view.go).
	backupViewMu       sync.Mutex
	backupExportStatus map[string]string

	// Test seams over platform calls.
	commitVocabulary        func() error
	deleteActiveTag         func(pluginID string) error
	deleteAutoScrollTag     func(pluginID string) error
	deleteCaretTag          func(pluginID string) error
	deleteFindTag           func(pluginID string) error
	deleteHintsTag          func(pluginID string) error
	deleteMediaActiveTag    func(pluginID string) error
	deleteQueryFieldTag     func(pluginID string) error
	deleteVideoModeTag      func(pluginID string) error
	dispatchPlatform        func(actionType string, params map[string]any) error
	appendEvent             func(eventType string, data map[string]any)
	dispatchPlatformRPC     func(actionType string, params map[string]any) error
	emitDisconnectNudge     func(pluginID, bundle string)
	exportDataFile          func(rel, name string) (string, error)
	frontmostBundle         func() (string, error)
	listHintRecords         func(collName string) ([]branchkit.CollectionRecord, error)
	hintsTagHeld            func(pluginID string) (bool, error)
	enumerateCommands       func() ([]branchkit.EnumeratedCommand, error)
	listPaletteSurfaceGates func() ([]string, error)
	listRunningBundles      func() (map[string]bool, error)
	listSpaces              func() ([]branchkit.SpaceInfo, error)
	livePrefixes            func(pluginID string) (map[string]bool, error)
	pushCommandGroupRPC     func(group string, cmds []branchkit.CommandSpec) (int, error)
	pressShortcut           func(s osShortcut) error
	putActiveTag            func(pluginID string) error
	putAutoScrollTag        func(pluginID string) error
	putCaretTag             func(pluginID string) error
	putFindTag              func(pluginID string) error
	putHintsTag             func(pluginID string) error
	putMediaActiveTag       func(pluginID string) error
	putQueryFieldTag        func(pluginID string) error
	putVideoModeTag         func(pluginID string) error
	replaceHintRPC          func(collName string, entries []branchkit.CollectionPutEntry, roles map[string]branchkit.FieldDisplay) error
	replacePaletteReserves  func(records []branchkit.CollectionPutEntry) error
	replacePaletteRows      func(records []branchkit.CollectionPutEntry) error
	replaceTabsCollection   func(records []branchkit.CollectionPutEntry) error
	reportProblem           func(p actionProblem)
	publishProblemDoc       func(doc branchkit.OutputState) error
	showProblemWindow       func() error
	refreshSettings         func()
	reportHealth            func(degraded bool, reason string)
	runAsync                func(f func())
	syncPerPrefix           func(pluginID, prefix string, newData map[string]any) error
	syncPerPrefixStrict     func(pluginID, prefix string, newData map[string]any) error
	syncPrefixList          func(pluginID string, newData map[string]any) error
	worldModel              func() (*branchkit.WorldModel, error)
}

func newHost(p *branchkit.Plugin) *Host {
	h := &Host{
		plugin:         p,
		calibration:    &calibrationFixture{},
		contributed:    map[string]connCatalog{},
		surgeryWaiters: map[string]chan surgeryReply{},
	}
	h.commitVocabulary = h.commitVocabularyDefault
	h.deleteActiveTag = h.deleteActiveTagDefault
	h.deleteAutoScrollTag = h.deleteAutoScrollTagDefault
	h.deleteCaretTag = h.deleteCaretTagDefault
	h.deleteFindTag = h.deleteFindTagDefault
	h.deleteHintsTag = h.deleteHintsTagDefault
	h.deleteMediaActiveTag = h.deleteMediaActiveTagDefault
	h.deleteQueryFieldTag = h.deleteQueryFieldTagDefault
	h.deleteVideoModeTag = h.deleteVideoModeTagDefault
	h.dispatchPlatform = h.dispatchPlatformAction
	h.appendEvent = h.appendEventDefault
	h.dispatchPlatformRPC = h.dispatchPlatformRPCDefault
	h.emitDisconnectNudge = h.emitDisconnectNudgeDefault
	h.exportDataFile = h.exportDataFileDefault
	h.frontmostBundle = h.frontmostBundleDefault
	h.listHintRecords = h.listHintRecordsDefault
	h.hintsTagHeld = h.hintsTagHeldDefault
	h.enumerateCommands = func() ([]branchkit.EnumeratedCommand, error) { return h.plugin.CommandsEnumerate() }
	h.listPaletteSurfaceGates = h.listPaletteSurfaceGatesDefault
	h.listRunningBundles = h.listRunningBundlesDefault
	h.listSpaces = h.plugin.NativeListSpaces
	h.livePrefixes = h.livePrefixesDefault
	h.pushCommandGroupRPC = h.pushCommandGroupRPCDefault
	h.pressShortcut = h.pressShortcutDefault
	h.putActiveTag = h.putActiveTagDefault
	h.putAutoScrollTag = h.putAutoScrollTagDefault
	h.putCaretTag = h.putCaretTagDefault
	h.putFindTag = h.putFindTagDefault
	h.putHintsTag = h.putHintsTagDefault
	h.putMediaActiveTag = h.putMediaActiveTagDefault
	h.putQueryFieldTag = h.putQueryFieldTagDefault
	h.putVideoModeTag = h.putVideoModeTagDefault
	h.replaceHintRPC = h.replaceHintRPCDefault
	h.replacePaletteReserves = h.replacePaletteReservesDefault
	h.replacePaletteRows = h.replacePaletteRowsDefault
	h.replaceTabsCollection = h.replaceTabsCollectionDefault
	h.reportProblem = h.reportProblemDefault
	h.publishProblemDoc = h.publishProblemDocDefault
	h.showProblemWindow = h.showProblemWindowDefault
	h.refreshSettings = h.refreshSettingsDefault
	h.reportHealth = h.reportHealthDefault
	h.runAsync = h.runAsyncDefault
	h.syncPerPrefix = h.syncPerPrefixDefault
	h.syncPerPrefixStrict = h.syncPerPrefixStrictDefault
	h.syncPrefixList = h.syncPrefixListDefault
	h.worldModel = func() (*branchkit.WorldModel, error) {
		return h.plugin.NativeWorldModel(branchkit.NativeWorldModelRequest{})
	}
	return h
}

// commitVocabularyDefault is the platform's vocabulary.commit — a refresh of
// the union grammar after the visible codeword set changed.
func (h *Host) commitVocabularyDefault() error { return h.plugin.VocabularyCommit() }
