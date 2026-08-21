package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

type wirelessHealthFake struct {
	values map[string]any
	errs   map[string]error
	calls  []string
}

func (f *wirelessHealthFake) Call(_ context.Context, object, method string,
	args, out any) error {
	key := object + "." + method
	if object == "file" && method == "read" {
		key += ":" + args.(map[string]string)["path"]
	}
	f.calls = append(f.calls, key)
	if err := f.errs[key]; err != nil {
		return err
	}
	value, ok := f.values[key]
	if !ok {
		return errors.New("unexpected call: " + key)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func wirelessRuntimeFixture(rows ...map[string]any) map[string]any {
	return map[string]any{"radio0": map[string]any{"interfaces": rows}}
}

func wirelessRuntimeRow(section, ifname, ssid string) map[string]any {
	return map[string]any{
		"section": section, "ifname": ifname,
		"config": map[string]any{
			"ssid": ssid,
			// A real response includes the plaintext key. The runtime decoder is
			// deliberately narrow and never carries it beyond JSON decoding.
			"key": "must-not-be-decoded",
		},
	}
}

func wirelessPlan(bsses ...wirelessRuntimeBSS) *wirelessRuntimePlan {
	return &wirelessRuntimePlan{desired: bsses}
}

func TestWirelessRuntimeRequiresEveryBandSection(t *testing.T) {
	fake := &wirelessHealthFake{values: map[string]any{
		"luci-rpc.getWirelessDevices": wirelessRuntimeFixture(
			wirelessRuntimeRow("guest_radio0", "wlan0-1", "Guest")),
		"hostapd.wlan0-1.get_status": map[string]any{"ssid": "Guest", "status": "ENABLED"},
	}}
	err := checkWirelessRuntimeOnce(context.Background(), fake, wirelessPlan(
		wirelessRuntimeBSS{section: "guest_radio0", ssid: "Guest"},
		wirelessRuntimeBSS{section: "guest_radio1", ssid: "Guest"},
	))
	if err == nil || !strings.Contains(err.Error(), "wireless.guest_radio1") {
		t.Fatalf("two-band plan with one missing BSS = %v", err)
	}
}

func TestWirelessRuntimeRejectsIgnoredBridgeIsolation(t *testing.T) {
	isolatedPath := "/sys/class/net/wlan0-1/brport/isolated"
	fake := &wirelessHealthFake{values: map[string]any{
		"luci-rpc.getWirelessDevices": wirelessRuntimeFixture(
			wirelessRuntimeRow("guest_radio0", "wlan0-1", "Guest")),
		"hostapd.wlan0-1.get_status": map[string]any{"ssid": "Guest", "status": "ENABLED"},
		"file.read:" + isolatedPath:  map[string]any{"data": "0\n"},
	}}
	err := checkWirelessRuntimeOnce(context.Background(), fake, wirelessPlan(
		wirelessRuntimeBSS{section: "guest_radio0", ssid: "Guest", isolated: true},
	))
	if err == nil || !strings.Contains(err.Error(), "isolated=\"0\"") ||
		!strings.Contains(err.Error(), "ignoring bridge_isolate") {
		t.Fatalf("bridge_isolate ignored = %v", err)
	}
}

func TestWirelessRuntimeChecksDuplicateSSIDSectionsIndividually(t *testing.T) {
	fake := &wirelessHealthFake{values: map[string]any{
		"luci-rpc.getWirelessDevices": wirelessRuntimeFixture(
			wirelessRuntimeRow("guest_radio0", "wlan0-1", "Guest"),
			wirelessRuntimeRow("guest_radio1", "wlan1-1", "Guest")),
		"hostapd.wlan0-1.get_status": map[string]any{"ssid": "Guest", "status": "ENABLED"},
		"hostapd.wlan1-1.get_status": map[string]any{"ssid": "Wrong", "status": "ENABLED"},
	}}
	err := checkWirelessRuntimeOnce(context.Background(), fake, wirelessPlan(
		wirelessRuntimeBSS{section: "guest_radio0", ssid: "Guest"},
		wirelessRuntimeBSS{section: "guest_radio1", ssid: "Guest"},
	))
	if err == nil || !strings.Contains(err.Error(), "wireless.guest_radio1") {
		t.Fatalf("duplicate SSID false-pass = %v", err)
	}
	if !wirelessCallsContain(fake.calls, "hostapd.wlan0-1.get_status") ||
		!wirelessCallsContain(fake.calls, "hostapd.wlan1-1.get_status") {
		t.Fatalf("not every duplicate-SSID section was checked: %v", fake.calls)
	}
}

func TestWirelessRuntimeNonIsolatedBSSBypassesSysfsProof(t *testing.T) {
	fake := &wirelessHealthFake{values: map[string]any{
		"luci-rpc.getWirelessDevices": wirelessRuntimeFixture(
			wirelessRuntimeRow("ordinary_radio0", "wlan0-1", "Ordinary")),
		"hostapd.wlan0-1.get_status": map[string]any{"ssid": "Ordinary", "status": "ENABLED"},
	}}
	err := checkWirelessRuntimeOnce(context.Background(), fake, wirelessPlan(
		wirelessRuntimeBSS{section: "ordinary_radio0", ssid: "Ordinary"},
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "file.read:") {
			t.Fatalf("non-isolated BSS required bridge sysfs proof: %v", fake.calls)
		}
	}
}

func TestWirelessRuntimeMissingIfnameDuringReloadIsRetryable(t *testing.T) {
	fake := &wirelessHealthFake{values: map[string]any{
		"luci-rpc.getWirelessDevices": wirelessRuntimeFixture(
			wirelessRuntimeRow("guest_radio0", "", "Guest")),
	}}
	err := checkWirelessRuntimeOnce(context.Background(), fake, wirelessPlan(
		wirelessRuntimeBSS{section: "guest_radio0", ssid: "Guest"},
	))
	if err == nil || !strings.Contains(err.Error(), "no runtime interface name yet") {
		t.Fatalf("missing transient ifname = %v", err)
	}
	if terminalWirelessRuntimeFailure(err) {
		t.Fatalf("missing transient ifname was terminal: %v", err)
	}
	if wirelessCallsContain(fake.calls, "hostapd..get_status") {
		t.Fatalf("empty ifname reached hostapd: %v", fake.calls)
	}

	fake = &wirelessHealthFake{values: map[string]any{
		"luci-rpc.getWirelessDevices": wirelessRuntimeFixture(
			wirelessRuntimeRow("guest_radio0", "../bad", "Guest")),
	}}
	err = checkWirelessRuntimeOnce(context.Background(), fake, wirelessPlan(
		wirelessRuntimeBSS{section: "guest_radio0", ssid: "Guest"},
	))
	if err == nil || !terminalWirelessRuntimeFailure(err) ||
		!strings.Contains(err.Error(), "unsafe runtime interface") {
		t.Fatalf("unsafe ifname = %v", err)
	}
}

func TestWirelessHealthWaitsForRuntimeIfnameToSettle(t *testing.T) {
	type rpcRequest struct {
		ID     int               `json:"id"`
		Params []json.RawMessage `json:"params"`
	}

	var inventoryCalls, statusCalls, isolationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if r.URL.Path != "/ubus" || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Params) < 4 {
			http.Error(w, "invalid ubus request", http.StatusBadRequest)
			return
		}
		var object, method string
		if json.Unmarshal(req.Params[1], &object) != nil || json.Unmarshal(req.Params[2], &method) != nil {
			http.Error(w, "invalid ubus target", http.StatusBadRequest)
			return
		}

		var payload any
		switch object + "." + method {
		case "network.interface.dump":
			payload = map[string]any{"interface": []map[string]any{{
				"interface": "lan", "up": true,
			}}}
		case "luci-rpc.getWirelessDevices":
			if inventoryCalls.Add(1) == 1 {
				// During a reload netifd can publish the desired UCI section
				// before it has assigned the BSS an interface name.
				payload = wirelessRuntimeFixture(map[string]any{
					"section": "guest_radio0",
					"config":  map[string]any{"ssid": "Guest"},
				})
			} else {
				payload = wirelessRuntimeFixture(
					wirelessRuntimeRow("guest_radio0", "wlan0-1", "Guest"))
			}
		case "hostapd.wlan0-1.get_status":
			statusCalls.Add(1)
			payload = map[string]any{"ssid": "Guest", "status": "ENABLED"}
		case "file.read":
			var args map[string]string
			if json.Unmarshal(req.Params[3], &args) != nil ||
				args["path"] != "/sys/class/net/wlan0-1/brport/isolated" {
				http.Error(w, "unexpected isolation path", http.StatusBadRequest)
				return
			}
			isolationCalls.Add(1)
			payload = map[string]any{"data": "1\n"}
		default:
			http.Error(w, "unexpected ubus call", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": []any{0, payload},
		})
	}))
	defer server.Close()

	client := ubus.New(ubus.Options{Host: strings.TrimPrefix(server.URL, "http://")})
	defer client.Close()
	plan := &reconcile.DevicePlan{Doc: render.Doc{Sections: []render.Section{{
		Config: "wireless", Type: "wifi-iface", Name: "guest_radio0",
		Values: map[string]string{"ssid": "Guest", "isolate": "1"},
	}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := healthCheck(plan)(ctx, client); err != nil {
		t.Fatalf("wireless health rejected a BSS that settled after reload: %v", err)
	}
	if got := inventoryCalls.Load(); got != 2 {
		t.Fatalf("wireless inventory calls = %d, want transient plus settled observation", got)
	}
	if got := statusCalls.Load(); got != 1 {
		t.Fatalf("hostapd status calls = %d, want one after ifname settled", got)
	}
	if got := isolationCalls.Load(); got != 1 {
		t.Fatalf("bridge isolation calls = %d, want one after ifname settled", got)
	}
}

func TestWirelessRuntimeDeniedIsolationProofNamesCurrentACL(t *testing.T) {
	isolatedPath := "/sys/class/net/wlan0-1/brport/isolated"
	fake := &wirelessHealthFake{
		values: map[string]any{
			"luci-rpc.getWirelessDevices": wirelessRuntimeFixture(
				wirelessRuntimeRow("guest_radio0", "wlan0-1", "Guest")),
			"hostapd.wlan0-1.get_status": map[string]any{"ssid": "Guest", "status": "ENABLED"},
		},
		errs: map[string]error{
			"file.read:" + isolatedPath: &ubus.DeniedError{Object: "file", Method: "read", Retried: true},
		},
	}
	err := checkWirelessRuntimeOnce(context.Background(), fake, wirelessPlan(
		wirelessRuntimeBSS{section: "guest_radio0", ssid: "Guest", isolated: true},
	))
	if err == nil || !terminalWirelessRuntimeFailure(err) ||
		!strings.Contains(err.Error(), "re-adopt") ||
		!strings.Contains(err.Error(), "/sys/devices/*/brport/isolated") {
		t.Fatalf("denied isolation proof = %v", err)
	}
}

func TestWirelessRuntimePlanIncludesEveryDesiredSectionAndDeletedSection(t *testing.T) {
	plan := wirelessRuntimePlanFor(&reconcile.DevicePlan{
		Doc: render.Doc{Sections: []render.Section{
			{Config: "wireless", Name: "guest_radio0", Values: map[string]string{"ssid": "Guest", "isolate": "1"}},
			{Config: "wireless", Name: "guest_radio1", Values: map[string]string{"ssid": "Guest", "isolate": "1"}},
		}},
		Plan: applyengine.Plan{Ops: []applyengine.Op{{
			Kind: applyengine.OpDelete, Config: "wireless", Section: "old_guest",
		}}},
	})
	if len(plan.desired) != 2 || len(plan.absent) != 1 || plan.absent[0] != "old_guest" {
		t.Fatalf("wireless runtime plan = %+v", plan)
	}
}

func wirelessCallsContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
