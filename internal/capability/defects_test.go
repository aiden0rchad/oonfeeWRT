package capability

import "testing"

// The mwlwifi PMF entry is the one this whole file was built around, and its
// confidence is load-bearing: the UI renders "[measured]" differently from
// "[documented]", and a reader deciding whether to believe a warning about
// their own hardware is exactly who that distinction is for.
//
// It began as ConfDeviceDoc — repeating OpenWrt's page. It is now measured,
// with a reproduction: PMF on, one forced 802.11r roam onto the 5 GHz radio,
// key installation failed, and 85 seconds later the firmware stopped answering
// and took every radio on the device with it. The same box, same boot, had run
// 14h50m with clients on that radio with PMF off.
//
// This test exists so that evidence cannot be quietly downgraded back to
// hearsay by an edit that only meant to reword something.
func TestTheMarvellPMFDefectIsMeasuredNotHearsay(t *testing.T) {
	var found *Defect
	for i := range knownDefects {
		if knownDefects[i].ID == "mwlwifi-80211w-unsupported" {
			found = &knownDefects[i]
			break
		}
	}
	if found == nil {
		t.Fatal("the mwlwifi PMF defect is gone from the registry")
	}
	if found.Confidence != ConfMeasuredHere {
		t.Errorf("confidence is %q, want %q — this one was reproduced on hardware",
			found.Confidence, ConfMeasuredHere)
	}
	if found.Severity != SevRadioDeath {
		t.Errorf("severity is %q; the measured outcome was the radio dying", found.Severity)
	}
	// The source must still point at something a sceptic can check, which is
	// the rule the registry states about itself.
	if found.Source == "" {
		t.Error("no source: every entry needs one a sceptical reader can check")
	}
}
