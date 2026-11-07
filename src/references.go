package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/branchkit/plugin-sdk-go"
)

var ()

// buildReferenceCommands returns the "name" command + (optionally) the
// "reference <name>" lookup command when refs exist. Pure — no RPC.
// Called from pushAllCommands.
func (h *Host) buildReferenceCommands(pluginID string) []branchkit.CommandSpec {
	appTag := "plugin." + pluginID + ".active"

	commands := []branchkit.CommandSpec{
		// Names come from the spelling-alphabet collection, not free text: a
		// reference is only useful if it can be re-spoken, and the closed
		// command engine can only hear words in its union — a bounded naming
		// vocabulary guarantees the round trip (and keeps the grammar free of
		// unbounded tails).
		branchkit.Command(branchkit.Word("name"), branchkit.Capture("", "alphabet")).
			Action(pluginID+".name_reference", map[string]any{"name": "{alphabet}"}).
			Description("Save element as named reference (pick an alphabet word)").
			Category("References").
			RequiresTags(appTag).
			Build(),
	}

	hasRefs := len(h.referenceNamesUnion()) > 0

	if hasRefs {
		refListName := "plugin." + pluginID + ".refs"
		commands = append(commands,
			branchkit.Command(branchkit.Word("reference"), branchkit.Capture("refs", refListName)).
				Action(pluginID+".resolve_reference", map[string]any{"name": "{refs}"}).
				Description("Activate saved reference").
				Category("References").
				RequiresTags(appTag).
				Build(),
		)
	}

	return commands
}

// updateReferenceNames records one connection's saved-reference names and
// publishes the union of every connection's set.
//
// The names are kept per connection because each browser posts the names in
// ITS OWN storage: one plugin-wide set, replaced by each post, let whichever
// browser reconnected last win, and a browser with no references posted an
// empty list that took the `reference <name>` command away from the other.
func (h *Host) updateReferenceNames(pluginID, connID string, names []string) error {
	h.refNamesMu.Lock()
	if len(names) == 0 {
		// An empty set is the absence of one: a key left behind would pile
		// up per reconnect for nothing, since the union treats it the same.
		delete(h.refNames, connID)
	} else {
		h.refNames[connID] = append([]string(nil), names...)
	}
	h.refNamesMu.Unlock()
	return h.publishReferenceNames(pluginID)
}

// dropReferenceNames forgets a connection's names, called when its stream
// closes, so a closed browser's names do not outlive it. Reports whether the
// connection had any (the caller then republishes the union). Caller may hold
// mu; this takes only refNamesMu.
func (h *Host) dropReferenceNames(connID string) bool {
	h.refNamesMu.Lock()
	defer h.refNamesMu.Unlock()
	if _, ok := h.refNames[connID]; !ok {
		return false
	}
	delete(h.refNames, connID)
	return true
}

// referenceNamesUnion is every live connection's names, de-duplicated and
// sorted, so the published set does not churn with map order.
func (h *Host) referenceNamesUnion() []string {
	h.refNamesMu.RLock()
	defer h.refNamesMu.RUnlock()
	seen := map[string]bool{}
	out := []string{}
	for _, names := range h.refNames {
		for _, n := range names {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// publishReferenceNames writes the union to plugin.<id>.refs and re-pushes
// the references group, whose `reference <name>` command exists only while
// the union is non-empty.
func (h *Host) publishReferenceNames(pluginID string) error {
	h.refPublishMu.Lock()
	defer h.refPublishMu.Unlock()
	names := h.referenceNamesUnion()

	// One record per name, id = name. Auto-registers as AsRecordKeyed on
	// first call so the matcher's <refs:plugin.browser.refs> capture sees
	// them.
	records := make([]branchkit.CollectionPutEntry, 0, len(names))
	for _, n := range names {
		raw, err := json.Marshal(map[string]string{"name": n})
		if err != nil {
			return fmt.Errorf("marshal reference %q: %w", n, err)
		}
		records = append(records, branchkit.CollectionPutEntry{ID: n, Payload: raw})
	}
	if err := h.replaceRefsCollection(pluginID, records); err != nil {
		return fmt.Errorf("replace reference list: %w", err)
	}

	// The reference command set changed shape (now includes or drops
	// "reference <name>") — re-push that group. The other sources are
	// untouched and stay live on their own.
	return h.pushCommandGroup(pluginID, groupReferences)
}

// replaceRefsCollectionDefault applies the name records via
// collection.replace. A seam for tests, like replaceTabsCollection.
func (h *Host) replaceRefsCollectionDefault(pluginID string, records []branchkit.CollectionPutEntry) error {
	_, err := h.plugin.Replace("plugin."+pluginID+".refs", records, branchkit.ScopeCollection())
	return err
}

// Phase 5a: plugin.<id>.references stored per-host (id = host).
// Each record's payload is {host, refs: {name: ref_data}}. The plugin
// reads one host's record, modifies the refs map, and puts it back.
type referencesHostRecord struct {
	Host string         `json:"host"`
	Refs map[string]any `json:"refs"`
}

func (h *Host) loadHostRecord(collName, host string) (*referencesHostRecord, error) {
	rec, err := h.plugin.Get(collName, host)
	if err != nil || rec == nil {
		return nil, err
	}
	var hr referencesHostRecord
	if err := json.Unmarshal(rec.Payload, &hr); err != nil {
		return nil, err
	}
	if hr.Refs == nil {
		hr.Refs = make(map[string]any)
	}
	return &hr, nil
}

func (h *Host) saveReferenceToCollection(pluginID, host, name string, ref map[string]any) error {
	collName := "plugin." + pluginID + ".references"

	hr, _ := h.loadHostRecord(collName, host)
	if hr == nil {
		hr = &referencesHostRecord{Host: host, Refs: make(map[string]any)}
	}
	hr.Refs[name] = ref
	return h.plugin.Put(collName, host, hr)
}

func (h *Host) deleteReferenceFromCollection(pluginID, host, name string) error {
	collName := "plugin." + pluginID + ".references"

	hr, err := h.loadHostRecord(collName, host)
	if err != nil {
		return err
	}
	if hr == nil {
		return nil
	}
	delete(hr.Refs, name)
	if len(hr.Refs) == 0 {
		_, _, err := h.plugin.DeleteMany(collName, []string{host})
		return err
	}
	return h.plugin.Put(collName, host, hr)
}

func (h *Host) loadReferencesForHost(pluginID, host string) (map[string]any, error) {
	collName := "plugin." + pluginID + ".references"

	hr, err := h.loadHostRecord(collName, host)
	if err != nil {
		return nil, err
	}
	if hr == nil {
		return nil, nil
	}
	return hr.Refs, nil
}
