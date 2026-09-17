package capability

import "testing"

func TestDiffKeepsExplicitSharedPHYRadiosDistinct(t *testing.T) {
	old := NewRegistry()
	old.Radios = []Radio{
		{Section: "radio0", Phy: "phy0", Device: "phy0.0-ap0", HWModes: []string{"b", "g", "n"}},
		{Section: "radio1", Phy: "phy0", Device: "phy0.1-ap0", HWModes: []string{"a", "n", "ac"}},
	}
	new := NewRegistry()
	new.Radios = []Radio{
		{Section: "radio0", Phy: "phy0", Device: "phy0.0-ap1", HWModes: []string{"b", "g", "n", "ax"}},
		{Section: "radio1", Phy: "phy0", Device: "phy0.1-ap1", HWModes: []string{"a", "n", "ac"}},
	}

	changes := radioChanges(old, new)
	if len(changes) != 1 || changes[0].Effect != EffectChanged || changes[0].Name != "radio0" {
		t.Fatalf("shared-PHY metadata change = %+v, want radio0 changed", changes)
	}
}

func TestDiffIgnoresInterfaceRenameWithExplicitSectionIdentity(t *testing.T) {
	old := NewRegistry()
	old.Radios = []Radio{{
		Section: "radio0", Phy: "phy0", Device: "phy0.0-ap0", HWModes: []string{"b", "g", "n"},
	}}
	new := NewRegistry()
	new.Radios = []Radio{{
		Section: "radio0", Phy: "phy0", Device: "phy0.0-ap7", HWModes: []string{"b", "g", "n"},
	}}
	if got := radioChanges(old, new); len(got) != 0 {
		t.Fatalf("interface-only rename changed hardware inventory: %+v", got)
	}
}

func TestDiffDoesNotInventChangeWhenUniqueLegacyInventoryGainsSections(t *testing.T) {
	old := NewRegistry()
	old.Radios = []Radio{
		{Phy: "phy0", Device: "phy0-ap0", HWModes: []string{"a", "n", "ac"}},
		{Phy: "phy1", Device: "phy1-ap0", HWModes: []string{"b", "g", "n"}},
	}
	new := NewRegistry()
	new.Radios = []Radio{
		{Section: "radio0", Phy: "phy0", Device: "phy0-ap0", HWModes: []string{"a", "n", "ac"}},
		{Section: "radio1", Phy: "phy1", Device: "phy1-ap0", HWModes: []string{"b", "g", "n"}},
	}
	if got := radioChanges(old, new); len(got) != 0 {
		t.Fatalf("first legacy-to-keyed reprobe invented changes: %+v", got)
	}
}

func TestDiffNamespacesMixedExplicitAndFallbackIdentities(t *testing.T) {
	old := NewRegistry()
	old.Radios = []Radio{
		{Section: "radio0", Phy: "phy0", Device: "phy0.0-ap0", HWModes: []string{"b", "g", "n"}},
		{Phy: "phy0", Device: "phy0.1-ap0", HWModes: []string{"a", "n", "ac"}},
	}
	new := NewRegistry()
	new.Radios = []Radio{
		{Section: "radio0", Phy: "phy0", Device: "phy0.0-ap1", HWModes: []string{"b", "g", "n"}},
		{Phy: "phy0", Device: "phy0.1-ap1", HWModes: []string{"a", "n", "ac", "ax"}},
	}
	changes := radioChanges(old, new)
	if len(changes) != 1 || changes[0].Effect != EffectChanged || changes[0].Name != "phy0" {
		t.Fatalf("mixed keyed/fallback diff = %+v, want fallback phy0 changed", changes)
	}
}

func TestDiffBridgesSectionMetadataPerRecord(t *testing.T) {
	for _, tc := range []struct {
		name          string
		beforeSection string
		afterSection  string
		afterModes    []string
		wantChanges   int
	}{
		{name: "gaining section unchanged", afterSection: "radio1", afterModes: []string{"n", "ac"}},
		{name: "gaining section with metadata change", afterSection: "radio1", afterModes: []string{"n", "ac", "ax"}, wantChanges: 1},
		{name: "losing section unchanged", beforeSection: "radio1", afterModes: []string{"n", "ac"}},
		{name: "losing section with metadata change", beforeSection: "radio1", afterModes: []string{"n", "ac", "ax"}, wantChanges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := NewRegistry()
			before.Radios = []Radio{
				{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Section: tc.beforeSection, Phy: "phy1", HWModes: []string{"n", "ac"}},
			}
			after := NewRegistry()
			after.Radios = []Radio{
				{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Section: tc.afterSection, Phy: "phy1", HWModes: tc.afterModes},
			}

			changes := radioChanges(before, after)
			if len(changes) != tc.wantChanges {
				t.Fatalf("changes = %+v, want %d", changes, tc.wantChanges)
			}
			if tc.wantChanges == 1 &&
				(changes[0].Effect != EffectChanged || changes[0].Name != "radio1") {
				t.Fatalf("metadata change = %+v, want radio1 changed", changes)
			}
		})
	}
}

func TestDiffIdentityLearningWithMissingModesDoesNotInventHardwareChange(t *testing.T) {
	for _, tc := range []struct {
		name          string
		beforeSection string
		afterSection  string
		beforeModes   []string
		afterModes    []string
	}{
		{name: "gain section and mode evidence", afterSection: "radio1", afterModes: []string{"n", "ac"}},
		{name: "lose section and mode evidence", beforeSection: "radio1", beforeModes: []string{"n", "ac"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := NewRegistry()
			before.Radios = []Radio{
				{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Section: tc.beforeSection, Phy: "phy1", HWModes: tc.beforeModes},
			}
			after := NewRegistry()
			after.Radios = []Radio{
				{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Section: tc.afterSection, Phy: "phy1", HWModes: tc.afterModes},
			}
			for _, change := range radioChanges(before, after) {
				if change.Effect == EffectLost || change.Effect == EffectGained {
					t.Errorf("identity learning invented hardware change: %+v", change)
				}
			}
		})
	}
}

func TestDiffEntirelyLegacyInventoriesStillCompareByUniquePHY(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{
		{Phy: "phy0", HWModes: []string{"b", "g", "n"}},
		{Phy: "phy1", HWModes: []string{"n", "ac"}},
	}
	after := NewRegistry()
	after.Radios = []Radio{
		{Phy: "phy0", HWModes: []string{"b", "g", "n"}},
		{Phy: "phy1", HWModes: []string{"n", "ac", "ax"}},
	}
	changes := radioChanges(before, after)
	if len(changes) != 1 || changes[0].Effect != EffectChanged || changes[0].Name != "phy1" {
		t.Fatalf("legacy metadata change = %+v, want phy1 changed", changes)
	}
}

func TestDiffReportsAmbiguousLegacySharedPHYWithoutHardwareClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before []Radio
		after  []Radio
	}{
		{
			name: "learning two sections",
			before: []Radio{
				{Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Phy: "phy0", HWModes: []string{"n", "ac"}},
			},
			after: []Radio{
				{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Section: "radio1", Phy: "phy0", HWModes: []string{"n", "ac"}},
			},
		},
		{
			name: "losing two sections",
			before: []Radio{
				{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Section: "radio1", Phy: "phy0", HWModes: []string{"n", "ac"}},
			},
			after: []Radio{
				{Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Phy: "phy0", HWModes: []string{"n", "ac"}},
			},
		},
		{
			name: "count changed while phy persists",
			before: []Radio{
				{Phy: "phy0", HWModes: []string{"b", "g", "n"}},
				{Phy: "phy0", HWModes: []string{"n", "ac"}},
			},
			after: []Radio{{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := NewRegistry(), NewRegistry()
			before.Radios, after.Radios = tc.before, tc.after
			changes := radioChanges(before, after)
			if len(changes) != 1 || changes[0].Effect != EffectAmbiguous || changes[0].Effect.Actionable() {
				t.Fatalf("ambiguous shared-PHY transition = %+v", changes)
			}
			for _, change := range changes {
				if change.Effect == EffectLost || change.Effect == EffectGained {
					t.Errorf("ambiguous transition claimed hardware change: %+v", change)
				}
			}
		})
	}
}

func TestDiffDoesNotMergeDifferentAuthoritativeSectionsOnSharedPHY(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{
		{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}},
		{Section: "radio1", Phy: "phy0", HWModes: []string{"n", "ac"}},
	}
	after := NewRegistry()
	after.Radios = []Radio{{Section: "radio0", Phy: "phy0", HWModes: []string{"b", "g", "n"}}}
	changes := radioChanges(before, after)
	if len(changes) != 1 || changes[0].Effect != EffectLost || changes[0].Name != "radio1" {
		t.Fatalf("authoritative shared-PHY loss = %+v, want radio1 lost", changes)
	}
}

func TestDiffPreservesEvidenceBackedAuthoritativeRename(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{{Section: "radio_old", Phy: "phy0", HWModes: []string{"n", "ac"}}}
	after := NewRegistry()
	after.Radios = []Radio{{Section: "radio_new", Phy: "phy0", HWModes: []string{"n", "ac"}}}
	changes := radioChanges(before, after)
	if len(changes) != 1 || changes[0].Effect != EffectRenamed ||
		changes[0].From != "radio_old" || changes[0].To != "radio_new" {
		t.Fatalf("authoritative rename = %+v", changes)
	}
}
