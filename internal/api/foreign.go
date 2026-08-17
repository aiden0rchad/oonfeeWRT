package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// The takeover brief: what it would take to bring a foreign SSID under
// management, said plainly, with the controller running none of it.
//
// # Why this prints a recipe instead of pressing the button
//
// Two automated designs for importing a foreign SSID were written and reviewed,
// and both failed the same way: each confirmed its own irreversible step with a
// health check that could not see what it claimed to prove. One would have let
// un-adopt delete a network the operator had before oonfeeWRT existed, with the
// restore "confirmed" by a check that short-circuits when there is nothing in
// the render to look for.
//
// The deeper reason is the ownership rule. A section is managed if and only if
// this controller wrote it and can put it back. That is what makes un-adopt a
// promise rather than a hope, and it is what stops a bug here from eating
// configuration a human made by hand. Automating a takeover means the
// controller deleting a section it did not write — the single thing the rule
// exists to forbid. So the operator does that part, on their own device, with
// their own hands, having read what it costs.
//
// # What the brief must never contain
//
// The passphrase, or anything derived from it. `foreignSection` has no field
// for a key and no field for whether one exists — the redaction is a property
// of the type rather than of somebody remembering to strip it, and a test
// marshals the whole response and asserts the lab passphrase appears nowhere in
// the bytes.
//
// Whether the SSID even has a passphrase is deliberately not reported. The poll
// does not collect it, and the only way to find out would be to read the
// operator's own key material off the device to answer a question the recipe
// already tells them how to answer themselves.
type foreignSection struct {
	Section string `json:"section"`
	SSID    string `json:"ssid"`
	Iface   string `json:"iface"`
	// Mode is the configured mode: "ap", "sta", "mesh". Empty when no poll has
	// read it, which is NOT the same as "ap".
	Mode string `json:"mode,omitempty"`
	// SafeToDisable is false for anything that is not a plain access point.
	//
	// A station or mesh section can be how the device reaches the network at
	// all. Advising someone to disable their own wireless uplink, over SSH,
	// with no rollback armed, is how a device is lost — and the controller
	// cannot tell from here whether this one is load-bearing.
	SafeToDisable bool   `json:"safe_to_disable"`
	Refusal       string `json:"refusal,omitempty"`

	// WouldStartBroadcasting names the OTHER devices that would begin
	// transmitting this SSID if it were recreated in the site model.
	//
	// Named individually rather than as a group name, because "all-aps" reads
	// as a label while "the WRT3200ACM would start broadcasting oonfee-c6-5g"
	// reads as a consequence. The site model has no per-device WLANs: a WLAN
	// belongs to a group and fans out to every device in it.
	WouldStartBroadcasting []string `json:"would_start_broadcasting,omitempty"`

	// Recipe is what the operator would run, on the device, themselves.
	Recipe []string `json:"recipe,omitempty"`
	// Cost is what they lose or accept by doing it.
	Cost []string `json:"cost,omitempty"`

	// Note is the decision already recorded about this section, if any.
	Note      string `json:"note,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
	DecidedAt int64  `json:"decided_at,omitempty"`
}

// buildBrief assembles the brief for one foreign BSS.
func buildBrief(b broadcastView, mode string, modesKnown bool,
	wouldBroadcast []string) foreignSection {

	f := foreignSection{
		Section: b.Section, SSID: b.SSID, Iface: b.Iface,
		WouldStartBroadcasting: wouldBroadcast,
	}
	if modesKnown {
		f.Mode = mode
	}

	switch {
	case !modesKnown || mode == "":
		f.SafeToDisable = false
		f.Refusal = "This device has not reported what mode this interface is " +
			"in, so there is no way to tell an access point from the link this " +
			"device uses to reach the network. No instructions are offered for " +
			"something that might be the only path to it."
	case mode != "ap":
		f.SafeToDisable = false
		f.Refusal = fmt.Sprintf("This interface is in %q mode, not \"ap\". It "+
			"may be how this device reaches the network at all — disabling it "+
			"could take the device off the network with no way back except "+
			"physical access. Nothing here will suggest that.", mode)
	default:
		f.SafeToDisable = true
		f.Recipe = []string{
			fmt.Sprintf("uci set wireless.%s.disabled='1'", b.Section),
			"uci commit wireless",
			// The reload is the step that matters and the one most likely to be
			// skipped. `uci commit` writes the file; it does not take a BSS off
			// the air. A brief that stopped at commit would have someone believe
			// the SSID was gone while it kept transmitting.
			"wifi reload",
		}
		f.Cost = []string{
			"Anything currently connected to this SSID is disconnected and will " +
				"not reconnect until you recreate it in oonfeeWRT.",
			"If it has a passphrase you need it to recreate the network. " +
				"oonfeeWRT does not store it, does not show it, and does not " +
				"read it — get it from the device with " +
				fmt.Sprintf("`uci get wireless.%s.key` before you start.", b.Section),
			"Recreating it here makes it a site-wide network, not a per-device " +
				"one. oonfeeWRT has no per-device WLANs.",
		}
		if len(wouldBroadcast) > 0 {
			f.Cost = append(f.Cost, fmt.Sprintf(
				"So %s would start broadcasting it too. If that is not what you "+
					"want, leave this alone and record why.",
				strings.Join(wouldBroadcast, " and ")))
		}
		f.Cost = append(f.Cost,
			"Verify with the on-air check afterwards rather than trusting the "+
				"config: a device can keep transmitting a configuration it no "+
				"longer has.")
	}
	return f
}

// handleForeignNote records or clears the decision about one foreign section.
//
// A recorded decision is the other half of this feature and the cheaper half:
// most foreign SSIDs should simply be left alone, and an operator who has
// decided that deserves to stop being asked. It writes nothing to any device.
func (s *Server) handleForeignNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	section := r.PathValue("section")
	if section == "" {
		writeErr(w, http.StatusBadRequest, "which section?")
		return
	}
	var req struct {
		SSID string `json:"ssid"`
		Note string `json:"note"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	if strings.TrimSpace(req.Note) == "" {
		if err := s.Store.ClearForeignNote(ctx, id, section); err != nil {
			writeErr(w, http.StatusInternalServerError, "could not clear the note")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
		return
	}
	who := "unknown"
	if sess, ok := sessionFrom(r.Context()); ok {
		who = sess.username
	}
	if err := s.Store.SetForeignNote(ctx, store.ForeignNote{
		DeviceID: id, Section: section, SSID: req.SSID, Note: req.Note,
		DecidedAt: s.now().Unix(), DecidedBy: who,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record the note")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true})
}

// sortedNames keeps the brief stable between requests.
func sortedNames(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
