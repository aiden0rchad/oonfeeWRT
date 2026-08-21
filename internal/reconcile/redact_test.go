package reconcile

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/render"
)

func TestSensitiveDriftRedactsBothValuesAtCollectionBoundary(t *testing.T) {
	const desired = "desired-passphrase-sentinel-3fK9"
	const live = "live-passphrase-sentinel-7qL2"
	sec := render.Section{
		Config: "wireless", Type: "wifi-iface", Name: "oowrt_wlan1_radio0",
		Values: map[string]string{
			render.OwnershipTag: "1",
			"key":               desired,
		},
	}
	drift := detectDrift(
		render.Doc{Sections: []render.Section{sec}},
		render.NewExisting(map[string]map[string]map[string]string{
			"wireless": {sec.Name: {render.OwnershipTag: "1", "key": live}},
		}),
		map[string]string{"wireless." + sec.Name: sec.Hash()},
	)
	if len(drift) != 1 {
		t.Fatalf("drift = %+v, want one key mismatch", drift)
	}
	got := drift[0]
	if got.Config != "wireless" || got.Section != sec.Name || got.Option != "key" {
		t.Fatalf("drift lost actionable identity: %+v", got)
	}
	if got.Ours != redactedUCIValue || got.Theirs != redactedUCIValue {
		t.Fatalf("raw values reached drift record: %+v", got)
	}

	apiText := got.String()
	previewBlob, err := json.Marshal(struct {
		Drift []string `json:"drift"`
	}{Drift: []string{apiText}})
	if err != nil {
		t.Fatal(err)
	}
	errorText := fmt.Errorf("preview drift: %s", got).Error()
	for surface, text := range map[string]string{
		"preview string": apiText,
		"API JSON":       string(previewBlob),
		"error string":   errorText,
	} {
		if strings.Contains(text, desired) || strings.Contains(text, live) {
			t.Errorf("%s leaked a credential: %s", surface, text)
		}
	}
	if !strings.Contains(apiText, "wireless."+sec.Name+".key") ||
		!strings.Contains(apiText, "values redacted") {
		t.Errorf("redacted drift is not actionable: %q", apiText)
	}
}

func TestSensitiveOptionClassifierIsExact(t *testing.T) {
	for _, option := range []string{
		"key", "key1", "wpa_psk", "preshared_key", "sae_password", "r0kh",
		"password", "passphrase", "auth_secret", "encryption_key",
		"private_key", "private_key_passwd",
	} {
		if !IsSensitiveOption(option) {
			t.Errorf("%q is not classified as sensitive", option)
		}
	}
	for _, option := range []string{
		"encryption", "key_index", "public_key", "password_policy", "monkey",
	} {
		if IsSensitiveOption(option) {
			t.Errorf("non-secret option %q was over-redacted", option)
		}
		if got := RedactOptionValue(option, "useful-diagnostic-value"); got != "useful-diagnostic-value" {
			t.Errorf("%q value = %q, want diagnostic value preserved", option, got)
		}
	}
}
