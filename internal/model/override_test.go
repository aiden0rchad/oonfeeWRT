package model

import "testing"

// The boundary is the product. A controller exists to keep the SSID, the
// passphrase, the security mode and the roaming configuration identical across
// every AP — those are exactly the settings that are miserable to keep
// consistent by hand and that fail confusingly when they drift. So they are not
// overridable at all, rather than overridable with a warning.
func TestSecurityAndRoamingAreNotOverridable(t *testing.T) {
	forbidden := []string{
		"ssid", "key", "security_mode", "encryption", "pmf",
		"ft", "ft_over_ds", "kv", "mobility_domain", "roaming",
		"network_id", "group_id", "bands",
	}
	for _, k := range forbidden {
		if OverrideKey(k).Valid() {
			t.Errorf("%q is overridable; a client roaming between APs that "+
				"disagree about it does not fail cleanly, it fails intermittently", k)
		}
		if _, _, err := ParseOverridePath("wlan.1." + k); err == nil {
			t.Errorf("path wlan.1.%s parsed; it should be refused", k)
		}
	}
	for _, k := range []OverrideKey{
		OverrideDisabled, OverrideHidden, OverrideIsolate, OverrideMaxAssoc,
	} {
		if !k.Valid() {
			t.Errorf("%q should be overridable — it varies legitimately per AP "+
				"and cannot desynchronise a client's view of the network", k)
		}
	}
}

func TestOverridePathRoundTrips(t *testing.T) {
	o := Override{DeviceID: 7, WLANID: 3, Key: OverrideHidden, Value: "1"}
	if o.Path() != "wlan.3.hidden" {
		t.Fatalf("path = %q", o.Path())
	}
	id, k, err := ParseOverridePath(o.Path())
	if err != nil || id != 3 || k != OverrideHidden {
		t.Errorf("round trip gave (%d, %q, %v)", id, k, err)
	}
	for _, bad := range []string{"", "wlan", "wlan.3", "wlan.x.hidden", "wlan.0.hidden",
		"radio.3.hidden", "wlan.3.hidden.extra"} {
		if _, _, err := ParseOverridePath(bad); err == nil {
			t.Errorf("%q parsed as an override path", bad)
		}
	}
}

// Applying overrides must not mutate the site model, or the second device
// rendered inherits the first device's deviations.
func TestApplyDoesNotMutateTheSiteModel(t *testing.T) {
	base := WLAN{ID: 1, SSID: "Home", Enabled: true,
		Options: WLANOptions{Hidden: false, MaxAssoc: 0}}
	ovs := Overrides{
		7: {{DeviceID: 7, WLANID: 1, Key: OverrideHidden, Value: "1"},
			{DeviceID: 7, WLANID: 1, Key: OverrideMaxAssoc, Value: "30"}},
	}

	seven, published := ovs.Apply(7, base)
	if !published || !seven.Options.Hidden || seven.Options.MaxAssoc != 30 {
		t.Fatalf("device 7 = %+v published=%v", seven.Options, published)
	}
	// The original is untouched...
	if base.Options.Hidden || base.Options.MaxAssoc != 0 {
		t.Errorf("Apply mutated the site model: %+v", base.Options)
	}
	// ...so a device with no overrides gets the site model.
	eight, published := ovs.Apply(8, base)
	if !published || eight.Options.Hidden || eight.Options.MaxAssoc != 0 {
		t.Errorf("device 8 inherited device 7's overrides: %+v", eight.Options)
	}
}

func TestDisabledOverrideUnpublishesAWLAN(t *testing.T) {
	base := WLAN{ID: 1, SSID: "Guest", Enabled: true}
	ovs := Overrides{7: {{DeviceID: 7, WLANID: 1, Key: OverrideDisabled, Value: "1"}}}
	if _, published := ovs.Apply(7, base); published {
		t.Error("a disabled override still published the WLAN")
	}
	// And the reverse: an override can publish a WLAN the site model disabled,
	// which is how you stage a network on one AP before rolling it out.
	off := WLAN{ID: 1, SSID: "Guest", Enabled: false}
	ovs = Overrides{7: {{DeviceID: 7, WLANID: 1, Key: OverrideDisabled, Value: "0"}}}
	if _, published := ovs.Apply(7, off); !published {
		t.Error("an explicit disabled=0 override did not publish the WLAN")
	}
}

// A malformed value must fail closed rather than quietly enabling something.
func TestMalformedOverrideValuesFailClosed(t *testing.T) {
	for _, v := range []string{"", "yes", "on", "banana", "2"} {
		o := Override{Key: OverrideHidden, Value: v}
		if o.Bool() {
			t.Errorf("value %q read as true; anything but 1/true must be false", v)
		}
	}
	for _, v := range []string{"", "-5", "lots", "3.5"} {
		if _, ok := (Override{Key: OverrideMaxAssoc, Value: v}).Int(); ok {
			t.Errorf("value %q parsed as a client limit", v)
		}
	}
	if n, ok := (Override{Key: OverrideMaxAssoc, Value: " 30 "}).Int(); !ok || n != 30 {
		t.Errorf("a padded number did not parse: %d %v", n, ok)
	}
}

// Every deviation has to be describable, because the risk of overrides is a
// fleet that drifts apart silently.
func TestEveryOverrideDescribesItself(t *testing.T) {
	for _, k := range []OverrideKey{
		OverrideDisabled, OverrideHidden, OverrideIsolate, OverrideMaxAssoc,
	} {
		for _, v := range []string{"1", "0", "30"} {
			d := Override{WLANID: 2, Key: k, Value: v}.Describe("Home")
			if d == "" {
				t.Errorf("%s=%s described as an empty string", k, v)
			}
			if len(d) < 10 {
				t.Errorf("%s=%s described as %q, which tells nobody anything", k, v, d)
			}
		}
	}
	// With no SSID to hand, it still says which WLAN.
	if d := (Override{WLANID: 9, Key: OverrideHidden, Value: "1"}).Describe(""); d == "" {
		t.Error("an override with no known SSID described as nothing")
	}
}
