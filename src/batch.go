package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// staleFrameSessionTTL is how long a frame session can sit untouched before
// buildTabPrefixState excludes its contributions from the cumulative per-tab
// rebuild. Tuned for dormant iframes (QuickBase's Pendo widget, ad iframes,
// modals that scroll off-screen) — the iframe ran a scan, committed strict
// codewords, then went silent, but the entries still leaked into the top
// frame's HUD as ghosts. Re-inclusion is self-healing: the next batch from
// that frame refreshes LastBatchAt and the rebuild picks it up again. The
// TTL is intentionally generous so a static-but-live iframe (no DOM churn
// for a stretch) isn't blinked out — half a minute is well past the typical
// scroll/mutation cadence on hint-heavy pages.
//
// The TOP frame is exempt (frameSessionStale). It is the page the user is
// looking at, not a widget that can go dormant behind it, and a static article
// sends no batch for minutes at a time. Aging it out meant the next incremental
// batch from an ad or chat iframe — or the sweeper, once that iframe was the
// only fresh session — rebuilt the tab without it: every top-frame codeword
// left `_strict` while its badge stayed painted and the hints tag stayed armed.
// A top frame leaves through its own ends instead: a frame session_end when the
// document goes, the tab deproject when focus moves.
const staleFrameSessionTTL = 30 * time.Second

// topFrameID is the frame id browsers give a tab's top-level document (Chrome
// and Firefox alike).
const topFrameID = 0

// frameSessionStale reports whether a frame session has gone dormant past the
// TTL and should drop out of the projection. Never true for the top frame.
func frameSessionStale(key frameKey, sess *FrameSession, now time.Time) bool {
	if key.FrameID == topFrameID || sess.LastBatchAt.IsZero() {
		return false
	}
	return now.Sub(sess.LastBatchAt) > staleFrameSessionTTL
}

// hintSuffixRoles is the display-role declaration for per-prefix hint
// collections (`browser_hints_<X>`). Each entry's value is the full
// payload object (id, frame_id, codeword, etc.); `letter` is what the
// discovery HUD wants as the subtitle, and the rest carry no display
// meaning.
var hintSuffixRoles = map[string]branchkit.FieldDisplay{
	"letter":   branchkit.FieldDisplaySecondary,
	"id":       branchkit.FieldDisplayPayload,
	"frame_id": branchkit.FieldDisplayPayload,
	"codeword": branchkit.FieldDisplayPayload,
	"type":     branchkit.FieldDisplayPayload,
}

// hintPrefixRoles is the display-role declaration for the prefix list
// (`browser_hints_prefix`). Each entry is an object {codeword, letter};
// `codeword` is the binding source for the dependent template
// `${prefix.codeword}`, and `letter` is what the HUD renders as the
// subtitle. The roles let surfaces consume the right field without the
// matcher having to read display roles.
var hintPrefixRoles = map[string]branchkit.FieldDisplay{
	"codeword": branchkit.FieldDisplayPrimary,
	"letter":   branchkit.FieldDisplaySecondary,
}

// syncPerPrefix makes one prefix's suffix collection contain exactly
// `newData`, via collection.replace — the platform computes the complement
// against what the collection actually holds, so there is no shadow to keep
// and nothing to lose across a plugin restart. This retires the third
// hand-rolled copy of the diff (after toolkit.ReplaceCollection's and the
// one that used to live here as computePrefixDiff/syncPrefixCollection);
// the byte-identical skip the shadow existed for is the verb's `skipped`
// semantics now, platform-side.
//
// `prefix` is the bare codeword (e.g. "gust"); the collection name is built
// as plugin.<id>.hints_<prefix>, auto-registered on first replace with
// this plugin as introducer.
//
// Declared as a var so tests can swap in a stub — tests assert against the
// (collection, data) pairs passed in. Must be called under `mu`.
func (h *Host) syncPerPrefixDefault(pluginID, prefix string, newData map[string]any) error {
	return h.replaceHintCollection(
		hintCollection(pluginID, prefix), newData, hintSuffixRoles,
	)
}

// syncPerPrefixStrict is the strict-viewport companion: same replace, but to
// `<plugin>_hints_<prefix>_strict`. Carries only entries whose element was in
// the strict viewport at last push — band-only entries still land in the
// base collection (via syncPerPrefix) and get badges painted by the
// extension, but are absent here so they don't match the activate command's
// dependent capture or appear in the Discovery HUD.
func (h *Host) syncPerPrefixStrictDefault(pluginID, prefix string, newData map[string]any) error {
	return h.replaceHintCollection(
		hintCollection(pluginID, prefix)+"_strict", newData, hintSuffixRoles,
	)
}

// replaceHintCollection is the shared body: marshal, replace with
// ScopeCollection (these are browser's own auto-registered collections, and
// the complement is computed over browser's own records regardless), and
// re-assert display metadata. Roles and the "Badge" label ride EVERY call —
// last-write-wins makes re-asserting free, and it repairs the metadata after
// a restart, where the old first-sync-only gating (keyed on an empty shadow)
// silently depended on process lifetime.
//
// A fingerprint of the exact bytes this would send short-circuits a sync that
// would change nothing. This IS process-local memory, which is what the
// migration away from shadows just deleted — so the distinction matters:
//
//	the shadow was CORRECTNESS state. It decided what to DELETE. Losing it
//	(process restart) produced wrong deletions — records the plugin no
//	longer published lingered forever, invisibly.
//
//	the fingerprint is OPTIMIZATION state. It decides whether to SKIP work
//	the platform would otherwise do redundantly. Losing it produces one
//	extra replace, which is idempotent and correct.
//
// A cache whose worst failure is "do the work anyway" is a different kind of
// object from a cache whose worst failure is "delete the wrong records". Only
// the second kind belongs on the platform's side of the boundary.
//
// Why it is worth having: the platform skips byte-identical RECORDS, but it
// can only do that after the full desired set has been marshaled in Go,
// written to the stdio pipe, parsed, and diffed against a writer-filtered
// list under the state lock. On the scroll path that is per affected prefix,
// twice (base + strict companion), for every grammar batch — and the
// reproject on every focus/tab change re-sends every prefix whether or not
// anything moved. The skip that costs nothing is the one that never reaches
// the wire.
func (h *Host) replaceHintCollection(
	collName string, newData map[string]any, roles map[string]branchkit.FieldDisplay,
) error {
	entries := make([]branchkit.CollectionPutEntry, 0, len(newData))
	for suffix, payload := range newData {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal %s suffix=%s: %w", collName, suffix, err)
		}
		entries = append(entries, branchkit.CollectionPutEntry{ID: suffix, Payload: raw})
	}

	fp := fingerprintEntries(entries)
	if prev, seen := h.state.LastSyncFingerprint[collName]; seen && prev == fp {
		return nil
	}

	if err := h.replaceHintRPC(collName, entries, roles); err != nil {
		// Do NOT record the fingerprint on failure: the next sync must
		// re-send. A fingerprint written before the wire call succeeded
		// would turn a transient RPC error into permanent divergence — the
		// one way this optimization could become a correctness bug.
		return fmt.Errorf("replace %s: %w", collName, err)
	}
	if h.state.LastSyncFingerprint != nil {
		h.state.LastSyncFingerprint[collName] = fp
	}
	h.noteSyncedKeysLocked(collName, fingerprintKeys(entries))
	return nil
}

// noteSyncedKeysLocked records the spoken-key fingerprint a successful sync
// (or the startup seed) left in collName, and marks the vocabulary dirty when
// it differs from the one recorded before — including when none was: an
// unknown prior state is treated as changed, so forgetting (a disconnect
// reset, a fresh process with nothing to seed from) costs one commit, never a
// missed one. Must be called under `mu`.
func (h *Host) noteSyncedKeysLocked(collName string, keyFP uint64) {
	if h.state.LastSyncKeyFingerprint == nil {
		h.state.LastSyncKeyFingerprint = make(map[string]uint64)
	}
	if prev, seen := h.state.LastSyncKeyFingerprint[collName]; !seen || prev != keyFP {
		h.state.HintVocabularyDirty = true
	}
	h.state.LastSyncKeyFingerprint[collName] = keyFP
}

// takeHintVocabularyDirty reports whether any hint sync changed a spoken-key
// set since the last call, and clears the mark. Takes `mu`.
func (h *Host) takeHintVocabularyDirty() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	dirty := h.state.HintVocabularyDirty
	h.state.HintVocabularyDirty = false
	return dirty
}

// fingerprintKeys hashes the ID set alone — the words a replace makes
// hearable. Same order-independent fold as fingerprintEntries, over empty
// payloads, so a payload-only change fingerprints the same here.
func fingerprintKeys(entries []branchkit.CollectionPutEntry) uint64 {
	keys := make([]branchkit.CollectionPutEntry, len(entries))
	for i, e := range entries {
		keys[i] = branchkit.CollectionPutEntry{ID: e.ID}
	}
	return fingerprintEntries(keys)
}

// seedHintSyncFingerprints learns what the platform already holds in this
// plugin's hint collections and records it as the sync fingerprints, so the
// first sync after a plugin restart skips every collection whose desired
// state the platform already has instead of re-sending all of them
// (a restart used to re-send every hint collection).
//
// Read through the existing surface only: collections.owned (livePrefixes)
// names the per-prefix collections holding our records, and ListAll reads
// each, plus the prefix list. The hint collections are memory-only, so after
// a platform restart there is nothing to read and nothing is seeded — the
// syncs then go out as they must. Records and their display roles live and
// die in the same app process, so a seeded skip never strands a
// collection without its roles.
//
// A seed is only a guess at the bytes the next replace would send: the
// payload is re-marshalled the way replaceHintCollection marshals it, and if
// the platform's rendering differs, the fingerprint misses and the replace
// goes out — the same outcome as not seeding. What a seed must never do is
// overwrite a claim a real sync made, so the whole seed runs under `mu`
// (syncs hold `mu` across their wire call) and only fills names that have no
// fingerprint yet.
func (h *Host) seedHintSyncFingerprints(pluginID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	live, err := h.livePrefixes(pluginID)
	if err != nil {
		branchkit.Logf(pluginID, "seed hint fingerprints: live prefixes unavailable: %v", err)
		return
	}
	names := make([]string, 0, 2*len(live)+1)
	names = append(names, hintCollection(pluginID, "prefix"))
	for word := range live {
		names = append(names, hintCollection(pluginID, word), hintCollection(pluginID, word)+"_strict")
	}
	if h.state.LastSyncFingerprint == nil {
		h.state.LastSyncFingerprint = make(map[string]uint64)
	}
	if h.state.LastSyncKeyFingerprint == nil {
		h.state.LastSyncKeyFingerprint = make(map[string]uint64)
	}
	seeded := 0
	for _, name := range names {
		if _, claimed := h.state.LastSyncFingerprint[name]; claimed {
			continue
		}
		recs, err := h.listHintRecords(name)
		if err != nil {
			branchkit.Logf(pluginID, "seed hint fingerprints: list %s: %v", name, err)
			continue
		}
		entries, ok := canonicalHintEntries(recs)
		if !ok {
			continue
		}
		h.state.LastSyncFingerprint[name] = fingerprintEntries(entries)
		h.state.LastSyncKeyFingerprint[name] = fingerprintKeys(entries)
		seeded++
	}
	branchkit.Logf(pluginID, "[INGEST] seeded hint sync fingerprints: %d of %d collections", seeded, len(names))
}

// canonicalHintEntries renders stored records as the entries
// replaceHintCollection would send for the same data: the payload decoded and
// re-marshalled (json.Marshal sorts object keys and compacts). False when a
// payload does not decode, in which case nothing is seeded for the
// collection.
func canonicalHintEntries(recs []branchkit.CollectionRecord) ([]branchkit.CollectionPutEntry, bool) {
	entries := make([]branchkit.CollectionPutEntry, 0, len(recs))
	for _, r := range recs {
		var payload any
		if err := json.Unmarshal(r.Payload, &payload); err != nil {
			return nil, false
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, false
		}
		entries = append(entries, branchkit.CollectionPutEntry{ID: r.ID, Payload: raw})
	}
	return entries, true
}

// listHintRecordsDefault is the seed's read: every record in one collection.
func (h *Host) listHintRecordsDefault(collName string) ([]branchkit.CollectionRecord, error) {
	return h.plugin.ListAll(collName)
}

// Test seam for the wire half, so the fingerprint skip above can be tested
// by counting calls. Production body is the replace itself.
func (h *Host) replaceHintRPCDefault(
	collName string, entries []branchkit.CollectionPutEntry, roles map[string]branchkit.FieldDisplay,
) error {
	_, err := h.plugin.Replace(
		collName, entries, branchkit.ScopeCollection(),
		branchkit.WithReplaceRoles(roles), branchkit.WithReplaceLabel("Badge"),
	)
	return err
}

// fingerprintEntries hashes the exact (id, payload) set a replace would send.
//
// Order-independent by construction: each entry is hashed alone and the
// results are XOR-folded, because `newData` is a Go map and its iteration
// order changes between runs — an order-sensitive hash would miss the skip
// almost every time and the optimization would silently do nothing.
//
// FNV-1a rather than a cryptographic hash: this guards a redundant RPC, not a
// security boundary, and a collision costs a skipped sync that the next
// genuine change corrects. The id is length-prefixed into the same hash as
// the payload so that moving a byte between them cannot alias.
func fingerprintEntries(entries []branchkit.CollectionPutEntry) uint64 {
	var acc uint64
	for _, e := range entries {
		h := fnv.New64a()
		fmt.Fprintf(h, "%d:%s", len(e.ID), e.ID)
		h.Write(e.Payload)
		acc ^= h.Sum64()
	}
	// Fold in the count so that a set and a strict subset sharing XOR parity
	// (two identical entries cancelling, say) cannot collide.
	return acc ^ uint64(len(entries))*0x9E3779B97F4A7C15
}

// livePrefixes asks the PLATFORM which per-prefix hint collections currently
// hold records this plugin owns, and returns the bare codewords.
//
// This replaces the live-prefix index — two `map[string]bool` maintained on
// every sync — which was the last process-local state in this plugin whose
// loss was a correctness problem. A restart forgot the prefixes the previous
// process had pushed, so their collections were never swept and lingered in
// the grammar until the platform restarted. Derived from what storage
// actually holds, there is nothing to forget, and the two indexes collapse
// into one derived set (the sweeps unioned them anyway).
//
// Both the base (`<plugin>_hints_<word>`) and strict
// (`<plugin>_hints_<word>_strict`) families map to the same bare codeword.
// The top-level `<plugin>_hints_prefix` list is excluded — nothing enumerates
// it. A codeword literally spelled "prefix" would alias with that name and be
// skipped; the alphabet is voice-owned so it isn't structurally impossible,
// and the cost is one un-swept collection rather than a wrong sweep.
//
// The platform omits collections holding no records of ours, so every prefix
// returned has something to wipe — the distinction the old boolean index made
// by deleting its key on an empty sync.
//
// SWEEP-frequency only (focus change, calibration apply/release). Not the
// scroll path: hold one result for the duration of a sweep and never cache it
// across sweeps — caching across sweeps is how the index this deletes came to
// exist. Declared as a var for the same test-seam reason as syncPerPrefix.
//
// Returns the error rather than an empty set, because "I could not find out"
// and "there is nothing there" are different answers and one caller acts on
// the difference: a release that swept nothing because the query failed must
// not report success, or the residue it left is invisible. The reproject is
// the other way round — it degrades to the focused source's own prefixes and
// self-heals on the next focus change.
func (h *Host) livePrefixesDefault(pluginID string) (map[string]bool, error) {
	owned, err := h.plugin.CollectionsOwned()
	if err != nil {
		return nil, fmt.Errorf("collections.owned: %w", err)
	}
	out := make(map[string]bool, len(owned))
	for _, o := range owned {
		if word, ok := hintPrefixOfCollection(pluginID, o.Name); ok {
			out[word] = true
		}
	}
	return out, nil
}

// hintPrefixOfCollection maps a collection name back to the bare codeword its
// per-prefix hint collection is named for. Both families fold to the same
// word: `<plugin>_hints_<word>` and `<plugin>_hints_<word>_strict`.
//
// Returns false for anything else, including the top-level
// `plugin.<id>.hints_prefix` list (nothing enumerates it) — which is also why a
// codeword literally spelled "prefix" would be skipped.
func hintPrefixOfCollection(pluginID, name string) (string, bool) {
	word, ok := strings.CutPrefix(name, hintCollection(pluginID, ""))
	if !ok || word == "prefix" {
		return "", false
	}
	word = strings.TrimSuffix(word, "_strict")
	return word, word != ""
}

// syncPrefixList makes the top-level plugin.browser.hints_prefix collection (the
// list of known prefix codewords) contain exactly `newData`. Same replace,
// different roles; needs no index of its own — it IS one collection, and
// nothing enumerates it process-side. Declared as a var for the same
// test-seam reason as syncPerPrefix. Must be called under `mu`.
func (h *Host) syncPrefixListDefault(pluginID string, newData map[string]any) error {
	return h.replaceHintCollection(hintCollection(pluginID, "prefix"), newData, hintPrefixRoles)
}

// Full-state grammar protocol. The extension sends PUT /grammar/frame with
// every codeword one frame holds; the plugin replaces that frame's map and
// re-syncs the affected prefixes. There are no deltas, so nothing on either
// side has to remember what the other was sent: a lost request is corrected by
// the next one, and a restart on either side by the first one after it.

// frameKey identifies one frame within one tab of one browser connection.
// Used as the key for the per-(conn, tab, frame) session map. ConnID is part
// of the key: tab ids are browser-LOCAL — Firefox
// numbers from 1, a second profile or fork numbers from 1 too — so two
// connected browsers can collide on (tab, frame). Before conn-scoping, a
// collision made ensureFrameSession read the other browser's session as
// "conn changed" and destructively thrash it. With the conn in the key,
// every browser's sessions coexist by construction, which is also what lets
// a browser-switch PARK the losing browser's sessions instead of wiping
// them (see recomputeFocusedSourceLocked).
type frameKey struct {
	ConnID  string
	TabID   int
	FrameID int
}

// FrameSession is one frame's last grammar request: what it holds, and when
// it last said so. The owning connection lives in the map key
// (frameKey.ConnID) — the single source; the per-prefix collections are global
// across every connected browser, so every cumulative rebuild and cleanup
// filters on it to keep one browser's codewords from leaking into another's
// grammar.
type FrameSession struct {
	// Identity of the DOCUMENT this session's batches come from, stamped
	// from GrammarFrameRequest.DocID on every request; changes only when a
	// new document takes over the (conn, tab, frame) key. A frame-scoped session_end carrying a different doc id is a
	// predecessor's late disconnect and must not tear this session down
	// (the doc-identity fence in handleHintsSessionEndPost).
	DocID string
	// codeword → element payload (id, letter, type, frame_id, codeword):
	// exactly the frame's last request, validated.
	Codewords map[string]map[string]string
	// Subset of Codewords whose element was in the strict viewport at
	// last request. Drives the `_strict` companion collections; band-only
	// entries (in Codewords, not in StrictCodewords) still get badges
	// painted by the extension but are not matchable.
	StrictCodewords map[string]bool
	// Wall-clock at the most recent request for this frame.
	// buildTabPrefixState skips sessions whose LastBatchAt is older than
	// `staleFrameSessionTTL` so dormant iframes can't ghost-contribute to
	// the cumulative per-prefix collections.
	LastBatchAt time.Time
	// True once the stale-session sweeper has reprojected this session out
	// of the collections; cleared on the next batch arrival. Keeps the
	// sweeper from rebuilding the projection on every tick while a dormant
	// session lingers in the map (sessions are kept so a frame that wakes,
	// or a tab that regains focus, reprojects with no extension traffic —
	// "stale" is not "deleted").
	SweptStale bool
}

// The grammar request's wire shapes live in the wire package (one declaration
// for both sides; the extension's gen.ts is generated from it).
type (
	GrammarElement       = wire.GrammarElement
	GrammarFrameRequest  = wire.GrammarFrameRequest
	GrammarFailure       = wire.GrammarFailure
	GrammarFrameResponse = wire.GrammarFrameResponse
)

// validateGrammarElement runs the per-element validation chain and
// builds the Put payload. Pure — no RPC — so it's directly testable
// without a platform connection. Returns a non-empty failReason if
// the element should be skipped; otherwise prefix/suffix/payload are
// what `plugin.Put` should receive.
func validateGrammarElement(e GrammarElement, frameID int, words [26]string) (prefix, suffix string, payload map[string]string, failReason string) {
	codeword := strings.TrimSpace(e.Codeword)
	if codeword == "" {
		return "", "", nil, "empty_codeword"
	}
	parts := strings.Fields(codeword)
	if len(parts) != 2 {
		// All hints are uniform two-word pairs (extension's label-pool.ts
		// buildPool comment); anything else is malformed.
		return "", "", nil, "invalid_codeword_parts"
	}
	letter := codewordToLetter(codeword, words)
	if letter == "" {
		return "", "", nil, "codeword_not_in_alphabet"
	}
	return parts[0], parts[1], map[string]string{
		"letter":   letter,
		"id":       strconv.Itoa(e.ID),
		"frame_id": strconv.Itoa(frameID),
		"codeword": codeword,
		"type":     e.Type,
	}, ""
}

// ensureFrameSession returns the FrameSession for key, allocating it on the
// frame's first request. Must be called under `mu`.
func (h *Host) ensureFrameSession(key frameKey) *FrameSession {
	if session := h.state.FrameSessions[key]; session != nil {
		return session
	}
	session := &FrameSession{
		Codewords:       make(map[string]map[string]string),
		StrictCodewords: make(map[string]bool),
		LastBatchAt:     time.Now(),
	}
	h.state.FrameSessions[key] = session
	return session
}

// cleanupFrameSessionLocked clears a frame session's tracked
// codewords and re-syncs the affected per-prefix collections for
// the tab. Each sync declares the post-cleanup desired state and the
// platform computes the complement — an empty post-cleanup map clears
// that prefix's collection. The caller is responsible for replacing
// state.FrameSessions[key] (or removing it entirely on session_end)
// BEFORE calling this so the rebuild reflects post-cleanup state.
//
// Must be called under `mu`. Returns the number of codewords that
// were tracked (and are now scheduled for removal from entity_cache
// via the sync's Delete diff).
func (h *Host) cleanupFrameSessionLocked(pluginID string, key frameKey, session *FrameSession, reason string) int {
	if session == nil || len(session.Codewords) == 0 {
		return 0
	}
	// Snapshot the prefixes this session contributed so we know what
	// to re-sync. After the caller drops this session, the rebuild
	// for these prefixes will reflect the absence of these entries.
	affectedPrefixes := make(map[string]bool)
	for codeword := range session.Codewords {
		parts := strings.Fields(codeword)
		if len(parts) == 2 {
			affectedPrefixes[parts[0]] = true
		}
	}
	count := len(session.Codewords)
	session.Codewords = make(map[string]map[string]string)
	session.StrictCodewords = make(map[string]bool)

	// Rebuild and sync each affected prefix from the post-cleanup
	// state, projected from the focused source so the canonical
	// collections reflect what the user is actually looking at. Falls
	// back to this session's own (conn, tab) while the focused source
	// is unknown. Scoped so a different browser's sibling frames never
	// bleed in.
	projConn, projTab := h.projectionSourceLocked(key.ConnID, key.TabID)
	h.resyncPrefixesLocked(pluginID, affectedPrefixes, projConn, projTab,
		fmt.Sprintf("session cleanup tab=%d", key.TabID))
	branchkit.Logf(pluginID,
		"session cleanup tab=%d frame=%d doc=%s reason=%s cleared=%d affected_prefixes=%d",
		key.TabID, key.FrameID, session.DocID, reason, count, len(affectedPrefixes))
	return count
}

// resyncPrefixesLocked re-syncs each affected prefix (base + strict + the
// prefix list) from the given projection source. Shared by the destructive
// frame cleanup and the keep-but-deproject tab path. Must be called under
// `mu`.
func (h *Host) resyncPrefixesLocked(pluginID string, affectedPrefixes map[string]bool, projConn string, projTab int, logCtx string) {
	allByPrefix, strictByPrefix := h.buildTabPrefixState(projConn, projTab)
	for prefix := range affectedPrefixes {
		data := allByPrefix[prefix]
		if data == nil {
			// No surviving entries: an empty desired state clears the
			// prefix's collection and drops it from the live-prefix
			// index.
			data = make(map[string]any)
		}
		if err := h.syncPerPrefix(pluginID, prefix, data); err != nil {
			branchkit.Logf(pluginID, "%s sync prefix=%s: %v", logCtx, prefix, err)
			continue
		}
		strictData := strictByPrefix[prefix]
		if strictData == nil {
			strictData = make(map[string]any)
		}
		if err := h.syncPerPrefixStrict(pluginID, prefix, strictData); err != nil {
			branchkit.Logf(pluginID, "%s strict sync prefix=%s: %v", logCtx, prefix, err)
		}
	}
	// Re-sync the prefix list. Entries are object-shaped so the
	// discovery HUD can render letter as a subtitle (secondary role)
	// while the activate pattern's `${prefix.codeword}` template pulls
	// the codeword for collection-name substitution.
	words := h.snapshotCodewords()
	prefixData := buildPrefixListData(allByPrefix, words)
	if err := h.syncPrefixList(pluginID, prefixData); err != nil {
		branchkit.Logf(pluginID, "%s sync prefix list: %v", logCtx, err)
	}
}

// cleanupTabSessionsLocked runs cleanupFrameSessionLocked for every
// frame in one connection's tab and removes their entries from
// state.FrameSessions. Used by handleHintsSessionEndPost when no
// frame_id is supplied (tab-wide session end). Conn-scoped: tab ids are
// browser-local, so "close tab 19" from one browser must not touch
// another browser's tab 19. Must be called under `mu`.
func (h *Host) cleanupTabSessionsLocked(pluginID, connID string, tabID int, reason string) int {
	cleared := 0
	// A closing tab can't send its chip-window release (range_pick.go); left
	// armed, the filter would survive into whatever next occupies this tab slot.
	if h.state.RangePickConnID == connID && h.state.RangePickTabID == tabID {
		h.drainRangePickLocked(pluginID, "tab_session_end")
	}
	for key, session := range h.state.FrameSessions {
		if key.ConnID != connID || key.TabID != tabID {
			continue
		}
		cleared += h.cleanupFrameSessionLocked(pluginID, key, session, reason)
		delete(h.state.FrameSessions, key)
	}
	return cleared
}

// deprojectTabSessionsLocked removes a tab's contribution from the live
// per-prefix collections WITHOUT destroying its frame sessions — the same
// keep-but-deproject pattern the stale sweeper uses. Sessions stay in
// state.FrameSessions so a later focus return reprojects them
// (reviveTabSessionsLocked + reprojectFocusedSourceLocked) with zero
// extension traffic — the read-time projection promise. The old destructive
// cleanup on tab_switch was a remnant of the push-time design: it emptied
// session.Codewords while the extension's delta-sync shadow stayed intact,
// so the refocus delta re-pushed nothing and most of the tab's codewords
// went voice-dead until an unrelated full republish. Found by the grammar
// epoch tripwire, 2026-06-12.
//
// If the projection source still points at this tab (the SW's session_end
// raced ahead of the focus recompute), the resync is skipped — the focus
// arm reprojects when focus actually lands elsewhere. Must be called
// under `mu`.
func (h *Host) deprojectTabSessionsLocked(pluginID, connID string, tabID int, reason string) int {
	count := 0
	affectedPrefixes := make(map[string]bool)
	for key, sess := range h.state.FrameSessions {
		if key.ConnID != connID || key.TabID != tabID {
			continue
		}
		count += len(sess.Codewords)
		// Park it the way the sweeper does: excluded from re-sweeps; the
		// next batch arrival or focus revival clears the flag.
		sess.SweptStale = true
		for codeword := range sess.Codewords {
			parts := strings.Fields(codeword)
			if len(parts) == 2 {
				affectedPrefixes[parts[0]] = true
			}
		}
	}
	if count == 0 {
		return 0
	}
	projConn, projTab := h.projectionSourceLocked(connID, tabID)
	if projConn == connID && projTab == tabID {
		branchkit.Logf(pluginID, "tab deproject deferred (still projected) tab=%d reason=%s kept=%d", tabID, reason, count)
		return count
	}
	h.resyncPrefixesLocked(pluginID, affectedPrefixes, projConn, projTab,
		fmt.Sprintf("tab deproject tab=%d", tabID))
	branchkit.Logf(pluginID,
		"tab deproject tab=%d reason=%s kept=%d affected_prefixes=%d (sessions retained for refocus reprojection)",
		tabID, reason, count, len(affectedPrefixes))
	return count
}

// reviveTabSessionsLocked re-arms a tab's kept-but-deprojected sessions for
// projection: clears SweptStale and refreshes LastBatchAt so
// buildTabPrefixState's dormant-TTL exclusion doesn't skip sessions that
// merely aged while their tab was in the background. Frames that truly died
// were removed by their frame-level session_end; anything dead that slips
// through re-ages past the TTL within one sweep interval. Must be called
// under `mu`.
func (h *Host) reviveTabSessionsLocked(connID string, tabID int) int {
	now := time.Now()
	revived := 0
	for key, sess := range h.state.FrameSessions {
		if key.ConnID != connID || key.TabID != tabID {
			continue
		}
		sess.SweptStale = false
		sess.LastBatchAt = now
		revived++
	}
	return revived
}

// cleanupConnSessionsLocked clears every frame session belonging to one
// browser connection and wipes its per-prefix collections. DESTRUCTIVE —
// called only when the connection is truly gone (disconnectSSEClient, and
// only while the ending stream still owns the conn_id: a same-conn_id
// reconnect keeps its sessions), so there is nothing these sessions could
// ever be revived for. A browser merely losing focus is PARKED
// instead (parkConnSessionsLocked) so a focus return reprojects with
// zero extension traffic. Must be called under `mu`. Returns the count
// of codewords removed.
func (h *Host) cleanupConnSessionsLocked(pluginID, connID, reason string) int {
	cleared := 0
	for key, session := range h.state.FrameSessions {
		if key.ConnID != connID {
			continue
		}
		cleared += h.cleanupFrameSessionLocked(pluginID, key, session, reason)
		delete(h.state.FrameSessions, key)
	}
	if cleared > 0 {
		branchkit.Logf(pluginID, "conn cleanup conn=%s reason=%s cleared=%d",
			connID, reason, cleared)
	}
	return cleared
}

// parkConnSessionsLocked removes a browser's contribution from the live
// per-prefix collections WITHOUT destroying its frame sessions — the
// conn-scope twin of deprojectTabSessionsLocked. Called on a browser→browser focus
// switch with the NEW focused source already set, so the resync rebuilds
// the collections from the winning browser and drains the loser's entries
// (its codewords must stop matching against the browser the user now
// sees). The loser's sessions stay parked (SweptStale, retained in
// state.FrameSessions, coexisting safely under conn-scoped keys) so a
// focus return reprojects them with zero extension traffic — this is
// what turns the switch-back recovery republish, and the
// recovery-per-tab-visited that followed it, into free reprojections.
// Must be called under `mu`. Returns the count of codewords parked.
func (h *Host) parkConnSessionsLocked(pluginID, connID string) int {
	count := 0
	affectedPrefixes := make(map[string]bool)
	for key, sess := range h.state.FrameSessions {
		if key.ConnID != connID {
			continue
		}
		count += len(sess.Codewords)
		// Parked the way the sweeper parks: excluded from re-sweeps; the
		// next batch arrival or focus revival clears the flag.
		sess.SweptStale = true
		for codeword := range sess.Codewords {
			parts := strings.Fields(codeword)
			if len(parts) == 2 {
				affectedPrefixes[parts[0]] = true
			}
		}
	}
	if count == 0 {
		return 0
	}
	projConn, projTab := h.projectionSourceLocked("", 0)
	h.resyncPrefixesLocked(pluginID, affectedPrefixes, projConn, projTab,
		fmt.Sprintf("conn park conn=%s", connID))
	branchkit.Logf(pluginID,
		"browser switch park conn=%s kept=%d affected_prefixes=%d (sessions retained for refocus reprojection)",
		connID, count, len(affectedPrefixes))
	return count
}

// buildPrefixListData builds the object-shaped prefix list payload from the
// current tab state. Each entry is `{codeword, letter}`: codeword is the
// spoken word (used by the activate pattern's `${prefix.codeword}` template
// for dependent-collection naming), letter is what the discovery HUD renders
// as the subtitle (via the secondary role on the collection's roles map).
//
// If the alphabet hasn't loaded yet (`words` is nil) we fall back to leaving
// `letter` empty — the push still succeeds and matching still works, the
// HUD just renders without a subtitle until the alphabet arrives. The
// scalar fallback in compute_subtitle does nothing for object-shaped values,
// so omitting the field is the only graceful option here.
func buildPrefixListData(allByPrefix map[string]map[string]any, words []string) map[string]any {
	var alphabet [26]string
	hasAlphabet := len(words) == 26
	if hasAlphabet {
		copy(alphabet[:], words)
	}
	prefixData := make(map[string]any, len(allByPrefix))
	for prefix := range allByPrefix {
		letter := ""
		if hasAlphabet {
			letter = codewordToLetter(prefix, alphabet)
		}
		prefixData[prefix] = map[string]string{
			"codeword": prefix,
			"letter":   letter,
		}
	}
	return prefixData
}

// buildTabPrefixState walks every frame session in the given tab that
// belongs to the given connection and returns two cumulative per-prefix
// suffix maps: `full` (every codeword the frames hold) and `strict` (the
// subset whose elements were in strict viewport at last push). Sibling
// frames in the same connection+tab are included so cross-frame codewords
// coexist in entity_cache; other connections are excluded so the global
// per-prefix namespace only ever reflects one browser at a time. The
// returned maps are the "desired post-state" inputs to syncPerPrefix and
// syncPerPrefixStrict respectively; each helper hands its map to
// collection.replace, which computes the complement platform-side.
//
// `strict` entries reuse the same payload object as `full` (no deep copy);
// neither caller mutates the payload between build and sync.
//
// Must be called under `mu`.
func (h *Host) buildTabPrefixState(connID string, tabID int) (full, strict map[string]map[string]any) {
	full = make(map[string]map[string]any)
	strict = make(map[string]map[string]any)
	now := time.Now()
	// Chip window (range_pick.go): while a "highlight" disambiguation pick owns
	// this tab, the chips own the codewords — project ONLY them, so the
	// Discovery HUD advertises exactly what the extension will accept.
	pickFilter := h.rangePickFilterLocked(connID, tabID)
	for key, sess := range h.state.FrameSessions {
		if key.ConnID != connID || key.TabID != tabID {
			continue
		}
		// Stale-iframe guard: a frame that stopped sending batches more than
		// `staleFrameSessionTTL` ago doesn't reflect the user's current view
		// — its element might be in a closed modal, a dismissed Pendo widget,
		// or an iframe that scrolled off-screen. Skipping it here also
		// causes the next syncPrefixCollection to emit Delete RPCs for the
		// suffixes it previously contributed, so HUD ghosts clear up too.
		if frameSessionStale(key, sess, now) {
			continue
		}
		for codeword, payload := range sess.Codewords {
			if pickFilter != nil && !pickFilter[codeword] {
				continue
			}
			parts := strings.Fields(codeword)
			if len(parts) != 2 {
				continue
			}
			prefix, suffix := parts[0], parts[1]
			converted := make(map[string]any, len(payload))
			for k, v := range payload {
				converted[k] = v
			}
			// A per-prefix collection is a SUFFIX menu: its entry KEY is the
			// suffix codeword, so its fields must be the SUFFIX's own. The
			// element payload carries the FULL pair (letter "fs", codeword
			// "flag same"); leaving those in makes a dependent-capture match
			// resolve {suffix.codeword}/{suffix.letter} to the whole pair, so
			// the hybrid activate's WORD_TO_LETTER dispatch gets "flag same"/"fs"
			// and clicks nothing. Override to the second word + its letter (the
			// hint is prefixLetter+suffixLetter, each one char, so drop the
			// leading prefix letter). id/frame_id/type stay for id resolution +
			// HUD payload. (2026-08-08, fixes the hybrid dispatch regression.)
			converted["codeword"] = suffix
			if pl, ok := converted["letter"].(string); ok && len(pl) >= 2 {
				converted["letter"] = pl[1:]
			}
			bucket, ok := full[prefix]
			if !ok {
				bucket = make(map[string]any)
				full[prefix] = bucket
			}
			bucket[suffix] = converted
			if sess.StrictCodewords[codeword] {
				sbucket, sok := strict[prefix]
				if !sok {
					sbucket = make(map[string]any)
					strict[prefix] = sbucket
				}
				sbucket[suffix] = converted
			}
		}
	}
	return full, strict
}

// projectionSourceLocked returns the (conn, tab) the canonical hint collections
// should be BUILT from: the currently-focused (conn, tab). Since the visible-bit
// migration this is no longer the admission GATE (batchIsFocusedSourceLocked
// decides that from the batch itself) — it is the read the reproject/deproject/
// sweep helpers use to know which source to rebuild, and FocusedTabID is kept
// authoritative by the batch that wins the gate (adoptFocusedTabFromBatchLocked)
// with the /active-tab fast-path as backup. Falls back to the supplied source
// while the focus binding is still pending — lone-browser fail-open, cold start,
// or an unbound fork — so single-browser use keeps working before the focused
// source is known. This fallback is load-bearing, not transitional:
// FocusedConnID stays "" until the focus handshake binds (primeFocusedBundle
// can't supply it at boot), so a lone browser's first batches project (and, via
// the reproject helpers, build) under the fallback until binding lands. Caller
// holds mu.
func (h *Host) projectionSourceLocked(fallbackConn string, fallbackTab int) (string, int) {
	if h.state.FocusedConnID != "" && h.state.FocusedTabID != 0 {
		return h.state.FocusedConnID, h.state.FocusedTabID
	}
	return fallbackConn, fallbackTab
}

// reprojectFocusedSourceLocked rebuilds the canonical per-prefix hint
// collections from the currently-focused source's FrameSessions, so the grammar
// follows OS focus / tab activation without waiting for the newly-focused
// source to re-send a grammar batch (the rescan round-trip). Prefixes the prior
// source left in the collections but the focused source no longer contributes
// are wiped, not stranded, by unioning the focused source's prefixes with those
// currently tracked in the live-prefix index. No-op until the focused source is
// fully known (leaves the collections untouched for the rescan to repopulate),
// and a no-op when the source has no stored grammar at all (see the empty-source
// guard below). Caller holds mu.
func (h *Host) reprojectFocusedSourceLocked(pluginID string) {
	h.reprojectSourceLocked(pluginID, "", 0)
}

// reprojectSourceLocked is reprojectFocusedSourceLocked with an explicit
// fallback source, for callers that know which source they mean when the focus
// binding is still unknown. Without it, a caller arriving during an unbound
// window (the plugin's FocusedConnID is "" until the focus handshake lands, and
// it re-enters that state across a service-worker restart) silently reprojects
// nothing. Caller holds mu.
func (h *Host) reprojectSourceLocked(pluginID, fallbackConn string, fallbackTab int) {
	conn, tab := h.projectionSourceLocked(fallbackConn, fallbackTab)
	if conn == "" || tab == 0 {
		return
	}
	allByPrefix, strictByPrefix := h.buildTabPrefixState(conn, tab)

	// Don't drain on an empty source. When the focused tab has no stored scan
	// data — a freshly focused/opened tab, or sessions a prior browser-switch
	// cleared — rebuilding from nothing would DeleteMany every live per-prefix
	// entry below (drain to prefixes=0), blanking the hints until the extension
	// re-scans; in that window a recognized codeword has no element to bind to,
	// so the user "says it again". Leave the current collections in place: the
	// incoming grammar batch (and the focus arm's reactivate rescan) update them.
	// Don't clear the screen because the next frame isn't ready. A genuine
	// element disappearance still drains via the frame's next grammar request
	// (handleGrammarFramePut), not this focus-driven rebuild — so the only thing
	// skipped here is the spurious wipe on a focus/tab change before the scan.
	if len(allByPrefix) == 0 {
		branchkit.Logf(pluginID, "[INGEST] reproject skipped (empty source) conn=%q tab=%d", conn, tab)
		return
	}

	// One enumeration for this whole sweep — the prefixes the platform says
	// we currently hold records under, unioned with the ones the focused
	// source contributes. Anything in the first set but not the second is a
	// prefix the prior source left behind, and gets wiped below rather than
	// stranded. Asked once here, deliberately not held across sweeps.
	live, err := h.livePrefixes(pluginID)
	if err != nil {
		// Best-effort: reproject what the focused source contributes and let
		// the next focus change sweep whatever this pass could not see.
		branchkit.Logf(pluginID, "reproject: live prefixes unavailable, stale prefixes not swept: %v", err)
	}
	prefixes := make(map[string]bool, len(allByPrefix)+len(live))
	for p := range allByPrefix {
		prefixes[p] = true
	}
	for p := range live {
		prefixes[p] = true
	}

	for prefix := range prefixes {
		data := allByPrefix[prefix]
		if data == nil {
			// Prefix no longer contributes; empty newData → the replace
			// clears the collection, and the next enumeration won't list it.
			data = make(map[string]any)
		}
		if err := h.syncPerPrefix(pluginID, prefix, data); err != nil {
			branchkit.Logf(pluginID, "reproject sync prefix=%s: %v", prefix, err)
			continue
		}
		strictData := strictByPrefix[prefix]
		if strictData == nil {
			strictData = make(map[string]any)
		}
		if err := h.syncPerPrefixStrict(pluginID, prefix, strictData); err != nil {
			branchkit.Logf(pluginID, "reproject strict sync prefix=%s: %v", prefix, err)
		}
	}

	words := h.snapshotCodewords()
	prefixData := buildPrefixListData(allByPrefix, words)
	if err := h.syncPrefixList(pluginID, prefixData); err != nil {
		branchkit.Logf(pluginID, "reproject sync prefix list: %v", err)
	}
	branchkit.Logf(pluginID, "[INGEST] reprojected focused source conn=%q tab=%d prefixes=%d",
		conn, tab, len(allByPrefix))
}

// Sweep cadence for stale frame sessions. A third of the TTL keeps the
// worst-case linger (TTL + one full interval) close to the TTL itself
// without making the idle tick meaningful work.
const staleSweepInterval = 10 * time.Second

// sweepStaleFrameSessions periodically reprojects the focused source when a
// frame session crosses staleFrameSessionTTL, so ghost entries leave the
// matchable collections (and the Discovery HUD) even when no further batch
// arrives to trigger a rebuild. Pre-sweeper, the TTL exclusion only took
// effect when an incidental sync touched the affected prefixes — on an
// otherwise-quiet page a dormant iframe's codewords lingered indefinitely.
//
// The all-stale case (a fully static page: every session past TTL) is left
// alone by reproject's empty-source guard, so a page the user is just
// reading does not go voice-dead at TTL. Runs as a goroutine from main.
func (h *Host) sweepStaleFrameSessions() {
	ticker := time.NewTicker(staleSweepInterval)
	for range ticker.C {
		h.sweepStaleFrameSessionsOnce(time.Now())
	}
}

// sweepStaleFrameSessionsOnce is one sweeper tick, as of `now`.
func (h *Host) sweepStaleFrameSessionsOnce(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn, tab := h.projectionSourceLocked("", 0)
	if conn == "" || tab == 0 {
		return
	}
	newlyStale := 0
	for key, sess := range h.state.FrameSessions {
		if key.ConnID != conn || key.TabID != tab || sess.SweptStale {
			continue
		}
		if len(sess.Codewords) == 0 {
			continue
		}
		if frameSessionStale(key, sess, now) {
			sess.SweptStale = true
			newlyStale++
		}
	}
	if newlyStale > 0 {
		branchkit.Logf(h.state.PluginID,
			"[INGEST] stale-session sweep: %d frame session(s) crossed TTL, reprojecting", newlyStale)
		h.reprojectFocusedSourceLocked(h.state.PluginID)
	}
}

// frameAdmission is the under-lock bookkeeping outcome of admitting a frame's
// grammar: the frame session it landed in and the projection decision.
type frameAdmission struct {
	session         *FrameSession
	pluginID        string
	projConn        string
	projTab         int
	isFocusedSource bool
	// Focus inputs snapshotted under the SAME mu-hold that computed
	// isFocusedSource, so the BK_STORED_FOCUSED_DRIFT tripwire in
	// respondStoredFrame asserts the invariant against a consistent view — a
	// concurrent /focus landing between admit and the tripwire can't make it
	// false-fire (it would be comparing against post-change state otherwise).
	focusedConn    string
	focusedWindow  int
	browserFocused bool
}

// frameCommit is the under-lock outcome of validating a frame's elements and
// replacing its session with them, plus (for the focused source) which source
// the publish steps rebuild the projection from.
//
// It carries the SOURCE, not a built snapshot. A snapshot built at commit time
// and published in a later lock hold went stale whenever two frames posted at
// once — A builds, B builds, B publishes, A publishes — and A's publish then
// removed the codewords B had just stored. Each publish step rebuilds from the
// sessions inside its own lock hold, so the last publish always reflects every
// commit before it.
type frameCommit struct {
	succeeded []string
	failed    []GrammarFailure
	// Prefixes the frame held before this request or holds after it: the ones
	// whose collections can have changed. An unchanged one costs nothing — the
	// replace's fingerprint skip stops it before the wire.
	affectedPrefixes map[string]bool
	project          bool
	projConn         string
	projTab          int
}

// handleGrammarFramePut receives one frame's whole grammar from the extension.
// The phases below each own their lock window (helpers acquire and release
// `mu` internally):
//
//	calibration short-circuit → alphabet snapshot → admit (locked)
//	→ skeleton → commit: validate + replace the frame's map (locked)
//	→ non-focused? store-and-respond : sync prefixes (locked)
//	→ roll back failed prefixes (locked) → prefix-list sync (locked)
//	→ vocabulary commit + hints tag.
func (h *Host) handleGrammarFramePut(_ *http.Request, reqp *GrammarFrameRequest) (GrammarFrameResponse, error) {
	req := *reqp

	// During a calibration trial, extension-driven writes would sync narrow
	// per-prefix state over the fixture-hydrated maximal alphabet × alphabet —
	// the replace would tear the trial's suffix entries out from under it.
	// Refuse without storing anything; the extension sends again later, and
	// since every request is complete, nothing is lost by refusing.
	if h.calibration.IsActive() {
		return GrammarFrameResponse{Result: "calibration_active", Failed: []GrammarFailure{}}, nil
	}

	h.refreshCodewords()
	h.codewordsMu.RLock()
	words := h.codewords
	h.codewordsMu.RUnlock()

	adm := h.admitGrammarFrame(&req)

	h.ensureSkeleton(adm.pluginID)

	commit := h.commitGrammarFrame(&req, adm, words)

	if !adm.isFocusedSource {
		return h.respondStoredFrame(&req, adm, commit), nil
	}

	pushFailures := h.syncAffectedPrefixes(adm.pluginID, commit)
	if len(pushFailures) > 0 {
		h.rollbackFailedPrefixes(adm.session, pushFailures, &commit)
	}

	h.syncTabPrefixList(adm.pluginID, commit, words)

	if h.takeHintVocabularyDirty() {
		// Only when some hint collection's spoken-key set changed — this
		// frame's syncs, or an earlier reproject/cleanup/park that does not
		// commit on its own (committing after every successful
		// frame made the platform rebuild and hash the word list under its
		// state lock only to find nothing new). Debounced (50 ms), so a burst
		// of changing frames still triggers one recognizer rebuild. Kept,
		// not deleted, after the display-grade demotion review (2026-07-19):
		// the desired union does change during browsing (browser-switch
		// wipes, tab-word churn), and dropping it would reopen the vocab-lag
		// class.
		h.scheduleVocabularyCommit(adm.pluginID)
	}

	codewordCount := h.finalizeSession(adm.session)

	branchkit.Logf(adm.pluginID,
		"grammar frame conn=%q tab=%d frame=%d elements=%d succeeded=%d failed=%d codewords=%d",
		req.ConnID, req.TabID, req.FrameID,
		len(req.Elements), len(commit.succeeded), len(commit.failed), codewordCount)

	h.emitGrammarFrameEvent(&req, commit, false)

	// Level-derive the hints tag from the post-commit focused projection — the
	// single policy point. Arms when this
	// focused-source request left the session holding codewords (badges shown,
	// in ANY visibility mode — visible ⇒ speakable), idles when a retraction
	// emptied it. Mode-agnostic, so manual-mode F/"show" arms too. Only the
	// focused source reaches here (non-focused returned above); nothing held.
	h.applyDerivedHintsTag("batch_commit")

	return GrammarFrameResponse{
		Result:             "ok",
		Failed:             nonNil(commit.failed),
		CommittedCodewords: codewordCount,
	}, nil
}

// nonNil: a reply's lists are [] on the wire, never null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// batchIsFocusedSourceLocked reports whether a grammar batch comes from the
// source the user is actually looking at: the OS-focused browser's OS-focused
// window's visible (foreground) tab. Three inputs, two of them carried by the
// batch itself so a stale side-channel can no longer strand a painting tab:
//
//   - conn == FocusedConnID — OS app-focus arbitration. Irreducibly plugin-side:
//     only the plugin sees cross-app focus via the OS bundle.
//   - WindowID == the conn's FocusedWindow — OS window-focus arbitration. The SW
//     sees window focus (chrome.windows.onFocusChanged) and reports it on /focus;
//     the batch stamps its own sender.tab.windowId. Distinguishes the focused
//     window from a side-by-side non-focused window of the SAME browser, both of
//     which hold a visible:true active tab.
//     The window signal is chrome.windows.onFocusChanged via /focus, not the
//     content script's document.hasFocus(): hasFocus goes false during
//     address-bar/devtools focus, which would kill hint speakability while the
//     user is looking at the page.
//   - Visible — the content script's own document.visibilityState. Distinguishes
//     the active tab from background tabs in the same window (both share WindowID).
//
// This replaces the old `TabID == projectionSourceLocked()` comparison, which
// pitted the batch's tab id against the lossy /active-tab cache — when that cache
// lagged, the painting tab's batch landed STORED (non-focused) and its badges
// went voice-dead until an unrelated focus transition. The batch now carries the
// bits that admit it, so that drift is impossible by construction.
//
// Fail-open at the edges, matching the retired projectionSourceLocked fallback:
//   - FocusedConnID == "" — the focus handshake hasn't bound yet (cold start,
//     lone browser); don't reject on conn.
//   - the conn's FocusedWindow == 0 or WindowID == 0 — the focused window isn't
//     known yet (no /focus since connect) or the SW couldn't stamp one; don't
//     gate on window. Visible still governs, so a background tab never projects.
//
// Must hold mu.
func (h *Host) batchIsFocusedSourceLocked(req *GrammarFrameRequest) bool {
	if h.state.FocusedConnID != "" && req.ConnID != h.state.FocusedConnID {
		return false
	}
	if fw := h.state.peek(req.ConnID).FocusedWindow; fw != 0 && req.WindowID != 0 && req.WindowID != fw {
		return false
	}
	return req.Visible
}

// batchWindowConfirmedLocked reports whether we POSITIVELY know this batch is
// from the connection's OS-focused window (both ids present and equal) — the
// stronger condition that gates the authoritative tab ADOPTION, distinct from
// batchIsFocusedSourceLocked's fail-open admission.
//
// Adoption reassigns FocusedTabID and deprojects the prior tab; doing that on a
// merely fail-open batch (focused window not yet known — cold start, SW restart)
// is unsafe when a browser has two visible windows: each window's active tab
// would fail-open, and they'd ping-pong FocusedTabID, thrashing deproject +
// reproject on every batch (storm-class churn) until /focus lands. So while the
// window is unknown we still PROJECT the batch (fail-open), but we do not adopt
// — FocusedTabID is left for the /focus + /active-tab fast-path to settle a beat
// later. The drift self-heal that motivated this migration is unaffected: it
// happens in steady state, where /focus has long since recorded the window.
// Must hold mu.
func (h *Host) batchWindowConfirmedLocked(req *GrammarFrameRequest) bool {
	fw := h.state.peek(req.ConnID).FocusedWindow
	return fw != 0 && req.WindowID != 0 && req.WindowID == fw
}

// adoptFocusedTabFromBatchLocked makes newTab the focused tab because a
// self-describing focused-source batch (the visible tab of the focused window)
// arrived for it while the plugin still had a different tab focused — a lost or
// lagging /active-tab, or genuine drift. The batch is authoritative, so we run
// the same tab-switch bookkeeping recomputeFocusedSourceLocked runs off the
// /active-tab fast-path: revive the new tab's kept sessions, park the old tab's
// projection, and reproject. Crucially it does NOT trigger a recovery rescan —
// the new tab's grammar is arriving in this very batch, so a rescan round-trip
// would be pure churn. Must hold mu.
func (h *Host) adoptFocusedTabFromBatchLocked(connID string, newTab int) {
	oldTab := h.state.FocusedTabID
	if oldTab == newTab {
		return
	}
	h.state.FocusedTabID = newTab
	// Keep the demoted /active-tab cache coherent so a later OS-app-focus
	// recompute doesn't revert FocusedTabID to the stale value it superseded.
	h.state.conn(connID).ActiveTab = newTab
	// Revive first so the new tab's kept-but-parked sessions are live before the
	// deproject/reproject below build the canonical collections from them.
	h.reviveTabSessionsLocked(connID, newTab)
	if oldTab != 0 {
		h.deprojectTabSessionsLocked(h.state.PluginID, connID, oldTab, "batch_visible_tab_switch")
	}
	h.reprojectFocusedSourceLocked(h.state.PluginID)
	branchkit.Logf(h.state.PluginID,
		"[FOCUS] focused tab adopted from batch conn=%q tab=%d->%d (self-describing visible batch superseded /active-tab); reproject follows",
		connID, oldTab, newTab)
}

// admitGrammarFrame records the request's arrival in its frame session and
// decides projection, under one `mu` hold.
//
// No cross-browser gate. Every accepted batch is STORED in its own
// FrameSession (keyed by conn+tab), but only the focused source's batches
// PROJECT into the global per-prefix collections (isFocusedSource here, decided
// by the self-describing batchIsFocusedSourceLocked predicate). A background
// browser's — or background window's, or background tab's — grammar therefore
// can no longer clobber the focused one: it sits in its session until it becomes
// the visible tab of the focused window, at which point reprojectFocusedSourceLocked
// rebuilds the collections from it. This replaces both the bound-bundle reject
// gate and the later tab-id-vs-/active-tab comparison with a decision the batch
// carries itself.
func (h *Host) admitGrammarFrame(req *GrammarFrameRequest) frameAdmission {
	h.mu.Lock()
	defer h.mu.Unlock()
	pluginID := h.state.PluginID
	h.state.HintVisibility = req.HintVisibility

	isFocusedSource := h.batchIsFocusedSourceLocked(req)

	// For the focused source the projection source IS this batch's (conn, tab).
	// If the batch names a tab the plugin didn't have as focused, it is the
	// authority — adopt it before we build the projection below. Gated on
	// batchWindowConfirmedLocked (not merely isFocusedSource): adoption is a
	// FocusedTabID reassignment + deproject, which two fail-open windows would
	// thrash before /focus records the focused window. While the window is
	// unknown we still project this batch, but leave FocusedTabID to the
	// /focus + /active-tab fast-path.
	projConn, projTab := req.ConnID, req.TabID
	if isFocusedSource && h.batchWindowConfirmedLocked(req) && req.TabID != h.state.FocusedTabID {
		h.adoptFocusedTabFromBatchLocked(req.ConnID, req.TabID)
	}

	key := frameKey{ConnID: req.ConnID, TabID: req.TabID, FrameID: req.FrameID}
	session := h.ensureFrameSession(key)
	session.LastBatchAt = time.Now()
	session.SweptStale = false
	// Bind the session to the document it now belongs to. On a document
	// handover (navigation reusing the frame key) the successor's first
	// batch re-stamps this, which is exactly what arms the doc-identity
	// fence against the predecessor's late session_end.
	if req.DocID != "" {
		session.DocID = req.DocID
	}
	return frameAdmission{
		session:         session,
		pluginID:        pluginID,
		projConn:        projConn,
		projTab:         projTab,
		isFocusedSource: isFocusedSource,
		focusedConn:     h.state.FocusedConnID,
		focusedWindow:   h.state.peek(req.ConnID).FocusedWindow,
		browserFocused:  h.state.BrowserFocused,
	}
}

// commitGrammarFrame validates the request's elements and makes them the
// frame's whole state, under one `mu` hold. Whatever the frame held before and
// does not hold now is simply gone: the prefixes it touched are in
// affectedPrefixes, so the sync that follows declares them without it.
func (h *Host) commitGrammarFrame(req *GrammarFrameRequest, adm frameAdmission, words [26]string) frameCommit {
	failed := make([]GrammarFailure, 0)
	succeeded := make([]string, 0, len(req.Elements))
	affectedPrefixes := make(map[string]bool, len(req.Elements))
	session := adm.session

	h.mu.Lock()
	defer h.mu.Unlock()
	for cw := range session.Codewords {
		if parts := strings.Fields(cw); len(parts) == 2 {
			affectedPrefixes[parts[0]] = true
		}
	}
	codewords := make(map[string]map[string]string, len(req.Elements))
	strict := make(map[string]bool)
	var drops hintDrops
	defer func() { drops.log(adm.pluginID, req.TabID, req.FrameID) }()
	for _, e := range req.Elements {
		prefix, suffix, payload, failReason := validateGrammarElement(e, req.FrameID, words)
		if failReason != "" {
			drops.add(failReason, e.Codeword)
			failed = append(failed, GrammarFailure{Codeword: e.Codeword, Reason: failReason})
			continue
		}
		codeword := prefix + " " + suffix
		codewords[codeword] = payload
		if e.InStrictViewport {
			strict[codeword] = true
		}
		succeeded = append(succeeded, codeword)
		affectedPrefixes[prefix] = true
	}
	session.Codewords = codewords
	session.StrictCodewords = strict
	// Only the focused source projects into the global collections. A
	// non-focused request is stored above and must not sync: a background
	// source would otherwise replace the focused source's entries.
	return frameCommit{
		succeeded:        succeeded,
		failed:           failed,
		affectedPrefixes: affectedPrefixes,
		project:          adm.isFocusedSource,
		projConn:         adm.projConn,
		projTab:          adm.projTab,
	}
}

// respondStoredFrame acknowledges a non-focused frame request: stored in its
// session, not projected.
func (h *Host) respondStoredFrame(req *GrammarFrameRequest, adm frameAdmission, commit frameCommit) GrammarFrameResponse {
	codewordCount := h.finalizeSession(adm.session)
	branchkit.Logf(adm.pluginID,
		"grammar frame STORED (non-focused) conn=%q tab=%d frame=%d elements=%d succeeded=%d failed=%d codewords=%d",
		req.ConnID, req.TabID, req.FrameID,
		len(req.Elements), len(commit.succeeded), len(commit.failed), codewordCount)

	// Identity-drift tripwire. Now an
	// INVARIANT ASSERTION that should be 0 by construction: after the visible-bit
	// migration the same self-describing predicate (batchIsFocusedSourceLocked)
	// decides both admission and this line, so a batch matching the focused
	// predicate — focused conn + focused window + visible — is admitted as the
	// focused source and never reaches respondStoredFrame. If one ever does, the
	// gate and the stored path disagree — a real logic bug. The old form compared
	// the batch's tab id against the lossy /active-tab cache; that drift class is
	// gone because the batch carries its own identity. A genuine background source
	// (wrong conn, wrong window, or hidden tab) is expected here and stays quiet.
	//
	// The focus inputs are the ones admit snapshotted under the SAME lock that
	// decided isFocusedSource — NOT a fresh read. A re-read here would false-fire
	// on a concurrent /focus that landed between admit and now (the batch was
	// correctly stored against the old window, then matches the new one). Against
	// the snapshot, a fire can only mean batchIsFocusedSourceLocked contradicted
	// itself, which is the actual invariant.
	if len(req.Elements) > 0 &&
		adm.browserFocused && req.ConnID == adm.focusedConn &&
		(adm.focusedWindow == 0 || req.WindowID == 0 || req.WindowID == adm.focusedWindow) && req.Visible {
		branchkit.Logf(adm.pluginID,
			"BK_STORED_FOCUSED_DRIFT focused-predicate batch was stored not projected: tab=%d window=%d conn=%q visible=%v — gate/stored disagreement (should be 0 by construction)",
			req.TabID, req.WindowID, req.ConnID, req.Visible)
	}

	h.emitGrammarFrameEvent(req, commit, true)
	return GrammarFrameResponse{
		Result:             "stored",
		Failed:             nonNil(commit.failed),
		CommittedCodewords: codewordCount,
	}
}

// syncAffectedPrefixes syncs each AFFECTED prefix under one `mu` hold.
// Affected = touched by this batch's adds or deletes. Other prefixes
// haven't changed and their entity_cache entries are still correct from
// prior batches. syncPerPrefix mutates state.LastSyncFingerprint, hence
// the lock. The desired state is rebuilt from the sessions INSIDE this hold
// (see batchCommit). Errors are captured per-prefix so the rollback step can
// roll back exactly the failed prefixes' session entries.
func (h *Host) syncAffectedPrefixes(pluginID string, commit frameCommit) map[string]error {
	pushFailures := make(map[string]error)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !commit.project {
		return pushFailures
	}
	allByPrefix, strictByPrefix := h.buildTabPrefixState(commit.projConn, commit.projTab)
	for prefix := range commit.affectedPrefixes {
		data := allByPrefix[prefix]
		if data == nil {
			// Prefix had entries before but this batch deleted all of
			// them. An empty desired state clears the collection and
			// drops the prefix from the live-prefix index.
			data = make(map[string]any)
		}
		if err := h.syncPerPrefix(pluginID, prefix, data); err != nil {
			pushFailures[prefix] = err
		}
		strictData := strictByPrefix[prefix]
		if strictData == nil {
			strictData = make(map[string]any)
		}
		if err := h.syncPerPrefixStrict(pluginID, prefix, strictData); err != nil {
			// A strict-collection failure doesn't tear down the badge
			// (extension paints from its own band state); log and continue.
			// Don't overwrite an existing base-sync failure for this prefix.
			if _, alreadyFailed := pushFailures[prefix]; !alreadyFailed {
				branchkit.Logf(pluginID, "strict sync prefix=%s: %v", prefix, err)
			}
		}
	}
	return pushFailures
}

// rollbackFailedPrefixes rolls back the session additions for each
// failed prefix and surfaces every affected codeword as failed, so the
// extension releases labels and skips painting. The session rollback
// keeps a future batch's sync from re-asserting these failed entries.
// Rewrites commit.succeeded / commit.failed in place, under `mu`.
func (h *Host) rollbackFailedPrefixes(session *FrameSession, pushFailures map[string]error, commit *frameCommit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	survivingSucceeded := commit.succeeded[:0]
	for _, cw := range commit.succeeded {
		parts := strings.Fields(cw)
		if len(parts) == 2 {
			if err, fail := pushFailures[parts[0]]; fail {
				delete(session.Codewords, cw)
				delete(session.StrictCodewords, cw)
				commit.failed = append(commit.failed, GrammarFailure{
					Codeword: cw,
					Reason:   "sync_failed: " + err.Error(),
				})
				continue
			}
		}
		survivingSucceeded = append(survivingSucceeded, cw)
	}
	commit.succeeded = survivingSucceeded
}

// syncTabPrefixList syncs the prefix-list collection with the full list
// of prefixes claimed across all frames in this tab. Same declare-the-
// desired-state pattern at the prefix-list scope, rebuilt from the sessions
// inside this lock hold for the same reason as syncAffectedPrefixes.
//
// Entries are object-shaped ({codeword, letter}): the activate
// pattern's `${prefix.codeword}` template binds the dependent
// collection name to the codeword field; the HUD pulls letter as
// the subtitle via the secondary role.
func (h *Host) syncTabPrefixList(pluginID string, commit frameCommit, words [26]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !commit.project {
		return
	}
	allByPrefix, _ := h.buildTabPrefixState(commit.projConn, commit.projTab)
	if len(allByPrefix) == 0 && len(commit.affectedPrefixes) == 0 {
		return
	}
	prefixData := buildPrefixListData(allByPrefix, words[:])
	if err := h.syncPrefixList(pluginID, prefixData); err != nil {
		branchkit.Logf(pluginID, "sync prefix list: %v", err)
	}
}

// finalizeSession returns the session's codeword count (a log breadcrumb),
// under `mu`.
//
// It used to set session.Complete on a final batch too. Nothing read that flag,
// so "finalize" only ever meant "count" — the mark it appeared to leave behind
// was never consulted by anything.
func (h *Host) finalizeSession(session *FrameSession) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(session.Codewords)
}

// emitGrammarFrameEvent emits the browser.grammar_batch observability event
// (the name predates full-state requests; consumers key on it). `stored` marks
// the non-focused stored-not-projected path.
func (h *Host) emitGrammarFrameEvent(req *GrammarFrameRequest, commit frameCommit, stored bool) {
	payload := map[string]any{
		"tab_id":    req.TabID,
		"frame_id":  req.FrameID,
		"elements":  len(req.Elements),
		"succeeded": len(commit.succeeded),
		"failed":    len(commit.failed),
	}
	if stored {
		payload["stored"] = true
	}
	h.emitEvent("browser.grammar_batch", payload)
}

// hintCollection names one of the per-prefix hint collections under the
// plugin's own namespace: `plugin.<id>.hints_<suffix>`. The family is
// declared in plugin.json as `plugin.browser.hints_*`; before 2026-09-14 it
// lived in the bare commons as `browser_hints_*`, a hand-rolled prefix the
// owned namespace could not express because it was sealed.
func hintCollection(pluginID, suffix string) string {
	return "plugin." + pluginID + ".hints_" + suffix
}
