package capability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func TestProbeRetainsAuthoritativeRadioSectionsOnSharedPHY(t *testing.T) {
	c := dial(t)
	ctx := context.Background()
	if err := c.Call(ctx, "__test", "reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Call(ctx, "__test", "reset", nil, nil) })
	if err := c.Call(ctx, "__test", "set_iwinfo_phys", map[string]any{
		"phys": map[string]string{"wlan0": "phy0", "wlan1": "phy0"},
	}, nil); err != nil {
		t.Fatal(err)
	}

	r, err := Probe(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Radios) != 2 {
		t.Fatalf("shared-PHY probe returned %d radios: %+v", len(r.Radios), r.Radios)
	}
	want := map[string]Radio{
		"radio0": {Section: "radio0", Device: "wlan0", Phy: "phy0", Frequency: 5180, Band: "5g"},
		"radio1": {Section: "radio1", Device: "wlan1", Phy: "phy0", Frequency: 2437, Band: "2g"},
	}
	for _, radio := range r.Radios {
		expected, ok := want[radio.Section]
		if !ok {
			t.Errorf("unexpected radio: %+v", radio)
			continue
		}
		if radio.Section != expected.Section || radio.Device != expected.Device ||
			radio.Phy != expected.Phy || radio.Frequency != expected.Frequency ||
			radio.Band != expected.Band {
			t.Errorf("radio %s = %+v, want %+v", radio.Section, radio, expected)
		}
	}
}

func TestProbeRetainsAuthoritativeSectionsForIdleRadios(t *testing.T) {
	c := dial(t)
	ctx := context.Background()
	if err := c.Call(ctx, "__test", "reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Call(ctx, "__test", "reset", nil, nil) })
	if err := c.Call(ctx, "__test", "disable_radios", nil, nil); err != nil {
		t.Fatal(err)
	}

	r, err := Probe(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Radios) != 2 {
		t.Fatalf("idle probe returned %d radios: %+v", len(r.Radios), r.Radios)
	}
	for _, radio := range r.Radios {
		field := reflect.ValueOf(radio).FieldByName("Section")
		if !field.IsValid() || field.String() != radio.Device || radio.Phy != radio.Device {
			t.Errorf("idle radio lost its configured name: %+v", radio)
		}
	}
}

func TestRadioEntriesOnlyCarryAuthoritativeSections(t *testing.T) {
	t.Run("authoritative map and unmapped extra", func(t *testing.T) {
		c := dial(t)
		ctx := context.Background()
		if err := c.Call(ctx, "__test", "reset", nil, nil); err != nil {
			t.Fatal(err)
		}
		entries, err := radiosWithInterfaces(ctx, c, []string{"wlan0", "wlan1", "unmapped0"})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"radio0": "radio0", "radio1": "radio1", "unmapped0": ""}
		assertEntrySections(t, entries, want)
	})

	t.Run("refused inventory fallback", func(t *testing.T) {
		c := dial(t)
		setACLGap(t, c, [2]string{"luci-rpc", "getWirelessDevices"})
		entries, err := radiosWithInterfaces(context.Background(), c, []string{"wlan0"})
		if err == nil {
			t.Fatal("refused inventory returned no error")
		}
		assertEntrySections(t, entries, map[string]string{"wlan0": ""})
	})

	t.Run("empty inventory fallback", func(t *testing.T) {
		c := emptyWirelessInventoryClient(t)
		entries, err := radiosWithInterfaces(context.Background(), c, []string{"wlan0"})
		if err != nil {
			t.Fatal(err)
		}
		assertEntrySections(t, entries, map[string]string{"wlan0": ""})
	})
}

func assertEntrySections(t *testing.T, entries []radioEntry, want map[string]string) {
	t.Helper()
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %d", entries, len(want))
	}
	for _, entry := range entries {
		field := reflect.ValueOf(entry).FieldByName("section")
		if !field.IsValid() || field.Kind() != reflect.String {
			t.Fatalf("radioEntry has no separate authoritative section field")
		}
		section, ok := want[entry.radio]
		if !ok {
			t.Errorf("unexpected radio entry %+v", entry)
			continue
		}
		if got := field.String(); got != section {
			t.Errorf("entry %q section = %q, want %q", entry.radio, got, section)
		}
	}
}

func emptyWirelessInventoryClient(t *testing.T) *ubus.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID, "result": []any{0, map[string]any{}},
		})
	}))
	t.Cleanup(server.Close)
	c := ubus.New(ubus.Options{Host: strings.TrimPrefix(server.URL, "http://")})
	t.Cleanup(c.Close)
	return c
}

func TestRadioSectionUsesEstablishedExportedNameJSON(t *testing.T) {
	type holder struct{ Radio Radio }
	value := holder{}
	field := reflect.ValueOf(&value.Radio).Elem().FieldByName("Section")
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.String {
		t.Fatal("Radio.Section is not an exported string")
	}
	field.SetString("radio_custom")
	b, err := json.Marshal(value.Radio)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"Section":"radio_custom"`) {
		t.Fatalf("Radio.Section JSON key changed established exported-name format: %s", b)
	}
}
