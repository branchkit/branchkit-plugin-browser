package main

import (
	"encoding/json"

	"github.com/branchkit/plugin-sdk-go"
)

// Typed, but deliberately NOT the generated EventsAppend wrapper: that one
// is built on p.Call, a blocking request/response, and this is fire-and-
// forget on a hot path that must not wait for the platform to answer.
//
// The pieces to be both typed AND async already exist without any new SDK
// surface — the generated request struct plus the method constant, handed
// to Notify. The platform dispatches id-less messages through the same
// handler and drops the result ("Notification from plugin: no id, has
// method" in plugins/rpc.rs), so the semantics are unchanged; what is
// gained is that a renamed method or a mistyped field is now a build error.
func (h *Host) emitEvent(eventType string, data map[string]any) {
	h.appendEvent(eventType, data)
}

// appendEventDefault sends the event to the platform. Seam: appendEvent.
func (h *Host) appendEventDefault(eventType string, data map[string]any) {
	raw, _ := json.Marshal(data)
	h.plugin.Notify(branchkit.MethodEventsAppend, &branchkit.EventsAppendRequest{
		EventType: eventType,
		Data:      json.RawMessage(raw),
	})
}
