package main

import (
	"encoding/json"
	"fmt"

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

	h.refNamesMu.RLock()
	hasRefs := len(h.refNames) > 0
	h.refNamesMu.RUnlock()

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

func (h *Host) updateReferenceNames(pluginID string, names []string) error {
	h.refNamesMu.Lock()
	h.refNames = names
	h.refNamesMu.Unlock()

	refListName := "plugin." + pluginID + ".refs"

	// Phase 5a: per-record collection.replace. Each name is one record;
	// id = name. Auto-registers as AsRecordKeyed on first call so the
	// matcher's <refs:plugin.browser.refs> capture sees them.
	records := make([]branchkit.CollectionPutEntry, 0, len(names))
	for _, n := range names {
		raw, err := json.Marshal(map[string]string{"name": n})
		if err != nil {
			return fmt.Errorf("marshal reference %q: %w", n, err)
		}
		records = append(records, branchkit.CollectionPutEntry{ID: n, Payload: raw})
	}
	if _, err := h.plugin.Replace(refListName, records, branchkit.ScopeCollection()); err != nil {
		return fmt.Errorf("replace reference list: %w", err)
	}

	// The reference command set changed shape (now includes "reference
	// <name>") — re-push that group. The other four sources are untouched
	// and stay live on their own.
	h.pushCommandGroup(pluginID, groupReferences)
	return nil
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
