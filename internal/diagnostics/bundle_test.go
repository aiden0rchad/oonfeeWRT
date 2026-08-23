package diagnostics

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerateProducesBoundedSecretFreeChecksummedArchive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "support.zip")
	now := time.Date(2026, 8, 22, 12, 34, 56, 0, time.FixedZone("PDT", -7*60*60))
	in := Input{
		GeneratedAt: now,
		Controller: ControllerSnapshot{
			Version: "v0.1.0 literal-secret-sentinel", Schema: 19, Platform: "linux/arm64",
			UptimeSeconds: 123, Health: "healthy cookie=cookie-secret-sentinel",
			MigrationState: "complete", IntegrityState: "ok", CollectedAt: now.Add(-time.Minute),
			Gaps: []string{"legacy service logs unavailable; password=gap-secret-sentinel"},
		},
		Devices: []DeviceSnapshot{{
			Identifier: "router-raw-id", Name: "Example Gateway", Model: "WRT3200ACM credential=device-credential-sentinel",
			Target: "mvebu/cortexa9", Firmware: "OpenWrt 25.12.5", Kernel: "6.12.94",
			PackageManager: "apk", CapabilityState: "acl present", LastObservedAt: now.Add(-2 * time.Minute),
			Gaps: []string{"wlan_key=wlan-gap-sentinel"},
		}},
		Sources: []SourceSnapshot{{
			Kind: "topology", DeviceIdentifier: "router-raw-id", State: "fresh",
			Detail: "client example-client at 192.0.2.249 or 2001:db8::dead:beef", ObservedAt: now.Add(-3 * time.Minute),
		}},
		Events: []EventSnapshot{
			{At: now.Add(-4 * time.Minute), Category: "audit", Severity: "info", DeviceIdentifier: "router-raw-id", Message: "password=unquoted password secret sentinel with spaces\npassphrase=unquoted passphrase secret sentinel with spaces\napi_key=api-key-secret-sentinel\nAuthorization: Basic YmFzaWMtc2VjcmV0LXNlbnRpbmVs\nsession_token=session-secret-sentinel\ncsrf=csrf-secret-sentinel"},
			{At: now.Add(-3 * time.Minute), Category: "wireless", Severity: "warning", Message: "example-client 02:00:00:ab:60:01 192.0.2.249 wlan_key=wlan-secret-sentinel totp=totp-secret-sentinel recovery_code=recovery-secret-sentinel"},
			{At: now.Add(-2 * time.Minute), Category: "system", Severity: "error", Message: "hash=$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$argon-hash-secret-sentinel\nshadow=$6$rounds=5000$salt$sha512-crypt-secret-sentinel\n-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-key-secret-sentinel\n-----END OPENSSH PRIVATE KEY-----"},
			{At: now.Add(-time.Minute), Category: "config", Severity: "info", Message: "wireless.@wifi-iface[0].key='raw-uci-secret-sentinel'"},
		},
		ControllerLogJSONL: []byte(strings.Join([]string{
			`{"level":"INFO","message":"example-client connected through Example Gateway (router-raw-id)","password":"json-password-secret-sentinel","csrf_token":"json-csrf-secret-sentinel"}`,
			`authorization: Bearer eyJaaaaaaaa.bbbbbbbb.cccccccc token=log-token-secret-sentinel`,
			`{"message":"literal-secret-sentinel","peer":"192.0.2.249","mac":"02:00:00:ab:60:01"}`,
		}, "\n")),
		Identifiers: []Identifier{
			{Kind: "client", Value: "example-client"},
			{Kind: "address", Value: "02:00:00:ab:60:01"},
			{Kind: "address", Value: "192.0.2.249"},
		},
		SecretValues: []string{"literal-secret-sentinel"},
	}

	result, err := Generate(context.Background(), path, in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != path || result.Size <= 0 || result.Size > MaxArchiveBytes {
		t.Fatalf("result=%+v", result)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode=%#o", stat.Mode().Perm())
	}
	if temps, _ := filepath.Glob(filepath.Join(dir, ".oonfeewrt-diagnostics-*.tmp")); len(temps) != 0 {
		t.Fatalf("temporary files remain: %v", temps)
	}

	members, order := readArchive(t, path)
	wantNames := []string{
		"README.txt", "manifest.json", "checksums.sha256", "controller/metadata.json",
		"devices/devices.json", "coverage/sources.json", "events/events.jsonl", "logs/controller.jsonl",
	}
	if strings.Join(order, "|") != strings.Join(wantNames, "|") {
		t.Fatalf("members=%v", order)
	}

	for name, data := range members {
		combined := strings.ToLower(name + "\n" + string(data))
		for _, forbidden := range []string{
			"secret-sentinel", "example-client", "router-raw-id", "example gateway",
			"02:00:00:ab:60:01", "192.0.2.249", "begin openssh", "$argon2id$",
			"2001:db8::dead:beef",
			"unquoted password secret sentinel", "unquoted passphrase secret sentinel", "ymfzawmtc2vjcmv0lxnlbnrpbmvs",
			"argon-hash-secret-sentinel", "sha512-crypt-secret-sentinel", "$6$rounds=5000",
		} {
			if strings.Contains(combined, forbidden) {
				t.Fatalf("%s leaked %q", name, forbidden)
			}
		}
		if len(data) > MaxMemberBytes {
			t.Fatalf("%s exceeds member limit: %d", name, len(data))
		}
	}

	var manifest Manifest
	if err := json.Unmarshal(members["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != FormatVersion || manifest.EvidenceMode != "stored_controller_evidence_only" || manifest.BundleID == "" {
		t.Fatalf("manifest=%+v", manifest)
	}
	for _, member := range manifest.Members {
		data, ok := members[member.Name]
		if !ok {
			t.Fatalf("manifest member missing: %s", member.Name)
		}
		sum := sha256.Sum256(data)
		if member.Size != len(data) || member.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("bad manifest checksum for %s", member.Name)
		}
	}
	verifyChecksumFile(t, members)

	clientAliases := regexp.MustCompile(`client-[0-9a-f]{12}`).FindAllString(string(members["events/events.jsonl"])+string(members["logs/controller.jsonl"])+string(members["coverage/sources.json"]), -1)
	if len(clientAliases) < 2 {
		t.Fatalf("client identifier was not consistently pseudonymized: %v", clientAliases)
	}
	for _, alias := range clientAliases[1:] {
		if alias != clientAliases[0] {
			t.Fatalf("identifier aliases differ within bundle: %v", clientAliases)
		}
	}
	routerAliases := regexp.MustCompile(`device-[0-9a-f]{12}`).FindAllString(string(members["README.txt"])+string(members["devices/devices.json"])+string(members["coverage/sources.json"])+string(members["events/events.jsonl"])+string(members["logs/controller.jsonl"]), -1)
	if len(routerAliases) < 5 {
		t.Fatalf("router identifier missing across members: %v", routerAliases)
	}
	for _, alias := range routerAliases[1:] {
		if alias != routerAliases[0] {
			t.Fatalf("router aliases differ within bundle: %v", routerAliases)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(members["logs/controller.jsonl"])), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("sanitized malformed JSONL is invalid: %q", line)
		}
	}
}

func TestGenerateUsesPerBundlePseudonyms(t *testing.T) {
	t.Parallel()
	in := minimalInput()
	in.Devices = []DeviceSnapshot{{Identifier: "same-router", Model: "model", LastObservedAt: in.GeneratedAt}}
	dir := t.TempDir()
	first := filepath.Join(dir, "first.zip")
	second := filepath.Join(dir, "second.zip")
	if _, err := Generate(context.Background(), first, in); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), second, in); err != nil {
		t.Fatal(err)
	}
	a := readDeviceID(t, first)
	b := readDeviceID(t, second)
	if a == "" || b == "" || a == b || a == "same-router" || b == "same-router" {
		t.Fatalf("per-bundle aliases=%q,%q", a, b)
	}
}

func TestGeneratePreservesStructuredMetadataMatchingDeviceName(t *testing.T) {
	t.Parallel()
	in := minimalInput()
	in.Devices = []DeviceSnapshot{{
		Identifier: "router-raw-id", Name: "Linksys WRT3200ACM", Model: "Linksys WRT3200ACM",
		Target: "mvebu/cortexa9 at 192.0.2.1", Firmware: "OpenWrt 25.12.5 token=firmware-secret",
		Kernel: "6.12.94 on 02:00:00:ab:60:01", LastObservedAt: in.GeneratedAt,
	}}
	path := filepath.Join(t.TempDir(), "metadata.zip")
	if _, err := Generate(context.Background(), path, in); err != nil {
		t.Fatal(err)
	}
	var devices []publicDevice
	if err := json.Unmarshal(readArchiveMember(t, path, "devices/devices.json"), &devices); err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Model != "Linksys WRT3200ACM" {
		t.Fatalf("structured model was changed: %+v", devices)
	}
	combined := string(readArchiveMember(t, path, "README.txt")) + string(readArchiveMember(t, path, "devices/devices.json"))
	if !strings.Contains(combined, "Linksys WRT3200ACM") {
		t.Fatal("structured model is absent from bundle")
	}
	for _, forbidden := range []string{"router-raw-id", "192.0.2.1", "02:00:00:ab:60:01", "firmware-secret"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("structured metadata leaked %q", forbidden)
		}
	}
}

func TestGenerateCancellationAndLimitsLeaveNoFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(dir, "cancelled.zip")
	if _, err := Generate(ctx, path, minimalInput()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled output exists: %v", err)
	}

	limited := minimalInput()
	limited.Devices = make([]DeviceSnapshot, MaxDevices+1)
	path = filepath.Join(dir, "limited.zip")
	if _, err := Generate(context.Background(), path, limited); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit err=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("limited output exists: %v", err)
	}
	if temps, _ := filepath.Glob(filepath.Join(dir, ".oonfeewrt-diagnostics-*.tmp")); len(temps) != 0 {
		t.Fatalf("temporary files remain: %v", temps)
	}

	partialPath := filepath.Join(dir, "partial.zip")
	if _, err := Generate(newCancelAfterContext(4), partialPath, minimalInput()); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-write cancel err=%v", err)
	}
	if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial output exists: %v", err)
	}
	if temps, _ := filepath.Glob(filepath.Join(dir, ".oonfeewrt-diagnostics-*.tmp")); len(temps) != 0 {
		t.Fatalf("temporary files remain after mid-write cancel: %v", temps)
	}
}

func TestGenerateRejectsEveryInputLimit(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Input){
		"devices":     func(in *Input) { in.Devices = make([]DeviceSnapshot, MaxDevices+1) },
		"sources":     func(in *Input) { in.Sources = make([]SourceSnapshot, MaxSources+1) },
		"events":      func(in *Input) { in.Events = make([]EventSnapshot, MaxEvents+1) },
		"identifiers": func(in *Input) { in.Identifiers = make([]Identifier, MaxIdentifiers+1) },
		"secrets":     func(in *Input) { in.SecretValues = make([]string, MaxSecretValues+1) },
		"log input":   func(in *Input) { in.ControllerLogJSONL = make([]byte, MaxControllerLogInputBytes+1) },
		"free text":   func(in *Input) { in.Controller.Health = strings.Repeat("x", MaxInputTextBytes+1) },
		"gaps":        func(in *Input) { in.Controller.Gaps = make([]string, MaxGaps+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := minimalInput()
			mutate(&in)
			path := filepath.Join(t.TempDir(), "rejected.zip")
			if _, err := Generate(context.Background(), path, in); !errors.Is(err, ErrLimit) {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected output exists: %v", err)
			}
		})
	}
}

func TestGenerateDoesNotOverwriteExistingOutput(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "existing.zip")
	const original = "keep me"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), path, minimalInput()); !errors.Is(err, ErrOutputExists) {
		t.Fatalf("err=%v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != original {
		t.Fatalf("existing output changed: %q, %v", got, err)
	}
}

func TestGenerateTailsControllerLogWithinBound(t *testing.T) {
	t.Parallel()
	in := minimalInput()
	prefix := strings.Repeat(`{"message":"discard me"}`+"\n", MaxControllerLogBytes/25)
	in.ControllerLogJSONL = []byte(prefix + `{"message":"retain me"}` + "\n")
	path := filepath.Join(t.TempDir(), "tail.zip")
	if _, err := Generate(context.Background(), path, in); err != nil {
		t.Fatal(err)
	}
	logs := readArchiveMember(t, path, "logs/controller.jsonl")
	if len(logs) > MaxControllerLogBytes || !strings.Contains(string(logs), "retain me") {
		t.Fatalf("log tail size=%d ending=%q", len(logs), logs[max(0, len(logs)-80):])
	}
}

func minimalInput() Input {
	now := time.Date(2026, 8, 22, 20, 0, 0, 0, time.UTC)
	return Input{
		GeneratedAt: now,
		Controller: ControllerSnapshot{
			Version: "v0.1.0", Schema: 19, Platform: "linux/amd64", UptimeSeconds: 10,
			Health: "healthy", MigrationState: "complete", IntegrityState: "ok", CollectedAt: now,
		},
	}
}

func readArchive(t *testing.T, path string) (map[string][]byte, []string) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	members := make(map[string][]byte, len(zr.File))
	order := make([]string, 0, len(zr.File))
	total := 0
	for _, file := range zr.File {
		if file.Mode().Perm() != 0o600 {
			t.Fatalf("member %s mode=%#o", file.Name, file.Mode().Perm())
		}
		if _, exists := members[file.Name]; exists {
			t.Fatalf("duplicate member %s", file.Name)
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(io.LimitReader(reader, MaxMemberBytes+1))
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		members[file.Name] = data
		order = append(order, file.Name)
		total += len(data)
		if total > MaxTotalBytes {
			t.Fatalf("archive exceeds total uncompressed limit: %d", total)
		}
	}
	return members, order
}

func readArchiveMember(t *testing.T, path, name string) []byte {
	t.Helper()
	members, _ := readArchive(t, path)
	data, ok := members[name]
	if !ok {
		t.Fatalf("missing member %s", name)
	}
	return data
}

func readDeviceID(t *testing.T, path string) string {
	t.Helper()
	var devices []publicDevice
	if err := json.Unmarshal(readArchiveMember(t, path, "devices/devices.json"), &devices); err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("devices=%v", devices)
	}
	return devices[0].ID
}

func verifyChecksumFile(t *testing.T, members map[string][]byte) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(members["checksums.sha256"])), "\n")
	got := make(map[string]string, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			t.Fatalf("bad checksum line %q", line)
		}
		got[parts[1]] = parts[0]
	}
	wantNames := make([]string, 0, len(members)-1)
	for name, data := range members {
		if name == "checksums.sha256" {
			continue
		}
		wantNames = append(wantNames, name)
		sum := sha256.Sum256(data)
		if got[name] != hex.EncodeToString(sum[:]) {
			t.Fatalf("checksum mismatch for %s", name)
		}
	}
	sort.Strings(wantNames)
	gotNames := make([]string, 0, len(got))
	for name := range got {
		gotNames = append(gotNames, name)
	}
	sort.Strings(gotNames)
	if strings.Join(wantNames, "|") != strings.Join(gotNames, "|") {
		t.Fatalf("checksum names=%v want %v", gotNames, wantNames)
	}
}

type cancelAfterContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining atomic.Int32
}

func newCancelAfterContext(checks int32) *cancelAfterContext {
	ctx, cancel := context.WithCancel(context.Background())
	out := &cancelAfterContext{Context: ctx, cancel: cancel}
	out.remaining.Store(checks)
	return out
}

func (c *cancelAfterContext) Err() error {
	if c.remaining.Add(-1) == 0 {
		c.cancel()
	}
	return c.Context.Err()
}
