// Package diagnostics builds bounded, redacted support bundles from already
// collected controller snapshots. It never performs network or router I/O.
package diagnostics

import (
	"archive/zip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	FormatVersion              = 1
	MaxDevices                 = 256
	MaxSources                 = 1024
	MaxEvents                  = 1000
	MaxIdentifiers             = 2048
	MaxPseudonyms              = 8192
	MaxGaps                    = 2048
	MaxSecretValues            = 512
	MaxFreeTextBytes           = 4 << 10
	MaxInputTextBytes          = 64 << 10
	MaxControllerLogInputBytes = 8 << 20
	MaxControllerLogBytes      = 2 << 20
	MaxMemberBytes             = 4 << 20
	MaxTotalBytes              = 16 << 20
	MaxArchiveBytes            = MaxTotalBytes + (1 << 20)
)

var (
	ErrLimit        = errors.New("diagnostics: input or output limit exceeded")
	ErrOutputExists = errors.New("diagnostics: output already exists")
)

type Input struct {
	GeneratedAt        time.Time
	Controller         ControllerSnapshot
	Devices            []DeviceSnapshot
	Sources            []SourceSnapshot
	Events             []EventSnapshot
	ControllerLogJSONL []byte
	Identifiers        []Identifier
	// SecretValues are known sensitive literals used only for redaction. They
	// are never serialized, hashed into the archive, or returned.
	SecretValues []string
}

type ControllerSnapshot struct {
	Version        string    `json:"version"`
	Schema         int       `json:"schema"`
	Platform       string    `json:"platform"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	Health         string    `json:"health"`
	MigrationState string    `json:"migration_state"`
	IntegrityState string    `json:"integrity_state"`
	CollectedAt    time.Time `json:"collected_at"`
	Gaps           []string  `json:"gaps,omitempty"`
}

type DeviceSnapshot struct {
	Identifier      string
	Name            string
	Model           string
	Target          string
	Firmware        string
	Kernel          string
	PackageManager  string
	CapabilityState string
	LastObservedAt  time.Time
	Gaps            []string
}

type SourceSnapshot struct {
	Kind             string
	DeviceIdentifier string
	State            string
	Detail           string
	ObservedAt       time.Time
}

type EventSnapshot struct {
	At               time.Time
	Category         string
	Severity         string
	DeviceIdentifier string
	Message          string
}

type Identifier struct {
	Kind  string
	Value string
}

type Manifest struct {
	FormatVersion int              `json:"format_version"`
	BundleID      string           `json:"bundle_id"`
	GeneratedAt   time.Time        `json:"generated_at"`
	EvidenceMode  string           `json:"evidence_mode"`
	Members       []ManifestMember `json:"members"`
	ChecksumFile  string           `json:"checksum_file"`
	Limits        Limits           `json:"limits"`
}

type ManifestMember struct {
	Name   string `json:"name"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

type Limits struct {
	Devices            int `json:"devices"`
	Sources            int `json:"sources"`
	Events             int `json:"events"`
	ControllerLogBytes int `json:"controller_log_bytes"`
	MemberBytes        int `json:"member_bytes"`
	TotalBytes         int `json:"total_bytes"`
}

type Result struct {
	Path     string
	Size     int64
	Manifest Manifest
}

type archiveMember struct {
	name string
	data []byte
}

type publicDevice struct {
	ID              string    `json:"id"`
	Model           string    `json:"model"`
	Target          string    `json:"target"`
	Firmware        string    `json:"firmware"`
	Kernel          string    `json:"kernel"`
	PackageManager  string    `json:"package_manager"`
	CapabilityState string    `json:"capability_state"`
	LastObservedAt  time.Time `json:"last_observed_at"`
	Gaps            []string  `json:"gaps,omitempty"`
}

type publicSource struct {
	Kind       string    `json:"kind"`
	DeviceID   string    `json:"device_id,omitempty"`
	State      string    `json:"state"`
	Detail     string    `json:"detail,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

type publicEvent struct {
	At       time.Time `json:"at"`
	Category string    `json:"category"`
	Severity string    `json:"severity"`
	DeviceID string    `json:"device_id,omitempty"`
	Message  string    `json:"message"`
}

var fixedMemberNames = map[string]struct{}{
	"README.txt":               {},
	"manifest.json":            {},
	"checksums.sha256":         {},
	"controller/metadata.json": {},
	"devices/devices.json":     {},
	"coverage/sources.json":    {},
	"events/events.jsonl":      {},
	"logs/controller.jsonl":    {},
}

// Generate writes a new mode-0600 ZIP at outputPath. The destination must not
// already exist. Partial files are removed on error or cancellation.
func Generate(ctx context.Context, outputPath string, in Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateInput(in); err != nil {
		return Result{}, err
	}

	pseudo, err := newPseudonymizer(ctx, in)
	if err != nil {
		return Result{}, err
	}
	redactor := newRedactor(pseudo, in.SecretValues)
	members, manifest, err := buildMembers(ctx, in, redactor, pseudo)
	if err != nil {
		return Result{}, err
	}

	if outputPath == "" {
		return Result{}, errors.New("diagnostics: output path is required")
	}
	if _, err := os.Lstat(outputPath); err == nil {
		return Result{}, ErrOutputExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("diagnostics: inspect output: %w", err)
	}
	dir := filepath.Dir(outputPath)
	tmp, err := os.CreateTemp(dir, ".oonfeewrt-diagnostics-*.tmp")
	if err != nil {
		return Result{}, fmt.Errorf("diagnostics: create temporary archive: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return Result{}, fmt.Errorf("diagnostics: protect temporary archive: %w", err)
	}
	if err := writeArchive(ctx, tmp, members); err != nil {
		return Result{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, fmt.Errorf("diagnostics: sync archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("diagnostics: close archive: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	stat, err := os.Stat(tmpName)
	if err != nil {
		return Result{}, fmt.Errorf("diagnostics: inspect archive: %w", err)
	}
	if stat.Size() > MaxArchiveBytes {
		return Result{}, ErrLimit
	}
	// A same-directory hard link publishes atomically without overwriting an
	// output that appeared after the Lstat check.
	if err := os.Link(tmpName, outputPath); err != nil {
		if _, statErr := os.Lstat(outputPath); statErr == nil {
			return Result{}, ErrOutputExists
		}
		return Result{}, fmt.Errorf("diagnostics: publish archive: %w", err)
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		_ = os.Remove(outputPath)
		return Result{}, fmt.Errorf("diagnostics: protect archive: %w", err)
	}
	return Result{Path: outputPath, Size: stat.Size(), Manifest: manifest}, nil
}

func validateInput(in Input) error {
	if in.GeneratedAt.IsZero() || in.Controller.CollectedAt.IsZero() || in.Controller.Schema < 0 || in.Controller.UptimeSeconds < 0 {
		return errors.New("diagnostics: invalid controller metadata")
	}
	if len(in.Devices) > MaxDevices || len(in.Sources) > MaxSources || len(in.Events) > MaxEvents ||
		len(in.Identifiers) > MaxIdentifiers || len(in.SecretValues) > MaxSecretValues ||
		len(in.ControllerLogJSONL) > MaxControllerLogInputBytes {
		return ErrLimit
	}
	secretBytes := 0
	for _, secret := range in.SecretValues {
		secretBytes += len(secret)
	}
	if secretBytes > MaxFreeTextBytes*4 {
		return ErrLimit
	}
	identifierBytes, gapCount := 0, len(in.Controller.Gaps)
	for _, identifier := range in.Identifiers {
		identifierBytes += len(identifier.Kind) + len(identifier.Value)
	}
	for _, device := range in.Devices {
		gapCount += len(device.Gaps)
	}
	if identifierBytes > MaxInputTextBytes*4 || gapCount > MaxGaps || !inputTextWithinBounds(in) {
		return ErrLimit
	}
	return nil
}

func inputTextWithinBounds(in Input) bool {
	values := []string{in.Controller.Version, in.Controller.Platform, in.Controller.Health, in.Controller.MigrationState, in.Controller.IntegrityState}
	values = append(values, in.Controller.Gaps...)
	for _, device := range in.Devices {
		values = append(values, device.Identifier, device.Name, device.Model, device.Target, device.Firmware, device.Kernel, device.PackageManager, device.CapabilityState)
		values = append(values, device.Gaps...)
	}
	for _, source := range in.Sources {
		values = append(values, source.Kind, source.DeviceIdentifier, source.State, source.Detail)
	}
	for _, event := range in.Events {
		values = append(values, event.Category, event.Severity, event.DeviceIdentifier, event.Message)
	}
	for _, identifier := range in.Identifiers {
		values = append(values, identifier.Kind, identifier.Value)
	}
	for _, value := range values {
		if len(value) > MaxInputTextBytes {
			return false
		}
	}
	return true
}

func buildMembers(ctx context.Context, in Input, r *redactor, p *pseudonymizer) ([]archiveMember, Manifest, error) {
	controller := in.Controller
	controller.Version = r.text(controller.Version)
	controller.Platform = r.text(controller.Platform)
	controller.Health = r.text(controller.Health)
	controller.MigrationState = r.text(controller.MigrationState)
	controller.IntegrityState = r.text(controller.IntegrityState)
	controller.GeneratedUTC()
	controller.Gaps = redactStrings(ctx, r, controller.Gaps)
	metadata, err := marshalJSON(controller)
	if err != nil {
		return nil, Manifest{}, err
	}

	devices := make([]publicDevice, 0, len(in.Devices))
	for _, device := range in.Devices {
		if err := ctx.Err(); err != nil {
			return nil, Manifest{}, err
		}
		devices = append(devices, publicDevice{
			ID:              p.alias(firstNonempty(device.Identifier, device.Name), "device"),
			Model:           r.metadata(device.Model),
			Target:          r.metadata(device.Target),
			Firmware:        r.metadata(device.Firmware),
			Kernel:          r.metadata(device.Kernel),
			PackageManager:  r.metadata(device.PackageManager),
			CapabilityState: r.metadata(device.CapabilityState),
			LastObservedAt:  utc(device.LastObservedAt),
			Gaps:            redactStrings(ctx, r, device.Gaps),
		})
	}
	deviceJSON, err := marshalJSON(devices)
	if err != nil {
		return nil, Manifest{}, err
	}

	sources := make([]publicSource, 0, len(in.Sources))
	for _, source := range in.Sources {
		if err := ctx.Err(); err != nil {
			return nil, Manifest{}, err
		}
		sources = append(sources, publicSource{
			Kind:       r.text(source.Kind),
			DeviceID:   p.alias(source.DeviceIdentifier, "device"),
			State:      r.text(source.State),
			Detail:     r.text(source.Detail),
			ObservedAt: utc(source.ObservedAt),
		})
	}
	sourceJSON, err := marshalJSON(sources)
	if err != nil {
		return nil, Manifest{}, err
	}

	eventJSONL, err := marshalEvents(ctx, in.Events, r, p)
	if err != nil {
		return nil, Manifest{}, err
	}
	logJSONL, err := sanitizeLogTail(ctx, in.ControllerLogJSONL, r)
	if err != nil {
		return nil, Manifest{}, err
	}
	readme := buildREADME(in, controller, devices, sources, r)

	payload := []archiveMember{
		{name: "README.txt", data: readme},
		{name: "controller/metadata.json", data: metadata},
		{name: "devices/devices.json", data: deviceJSON},
		{name: "coverage/sources.json", data: sourceJSON},
		{name: "events/events.jsonl", data: eventJSONL},
		{name: "logs/controller.jsonl", data: logJSONL},
	}
	if err := checkMembers(payload); err != nil {
		return nil, Manifest{}, err
	}

	manifest := Manifest{
		FormatVersion: FormatVersion,
		BundleID:      p.bundleID,
		GeneratedAt:   utc(in.GeneratedAt),
		EvidenceMode:  "stored_controller_evidence_only",
		ChecksumFile:  "checksums.sha256",
		Limits: Limits{
			Devices: MaxDevices, Sources: MaxSources, Events: MaxEvents,
			ControllerLogBytes: MaxControllerLogBytes, MemberBytes: MaxMemberBytes, TotalBytes: MaxTotalBytes,
		},
	}
	for _, member := range payload {
		manifest.Members = append(manifest.Members, manifestMember(member))
	}
	manifestJSON, err := marshalJSON(manifest)
	if err != nil {
		return nil, Manifest{}, err
	}
	checksums := checksumFile(append(payload, archiveMember{name: "manifest.json", data: manifestJSON}))
	members := []archiveMember{payload[0], {name: "manifest.json", data: manifestJSON}, {name: "checksums.sha256", data: checksums}}
	members = append(members, payload[1:]...)
	if err := checkMembers(members); err != nil {
		return nil, Manifest{}, err
	}
	return members, manifest, nil
}

func writeArchive(ctx context.Context, dst io.Writer, members []archiveMember) error {
	zw := zip.NewWriter(dst)
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			_ = zw.Close()
			return err
		}
		if _, ok := fixedMemberNames[member.name]; !ok || strings.Contains(member.name, "..") || strings.HasPrefix(member.name, "/") {
			_ = zw.Close()
			return errors.New("diagnostics: unsafe archive member")
		}
		header := &zip.FileHeader{Name: member.name, Method: zip.Deflate}
		header.SetMode(0o600)
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		entry, err := zw.CreateHeader(header)
		if err != nil {
			_ = zw.Close()
			return fmt.Errorf("diagnostics: create archive member: %w", err)
		}
		if err := writeWithContext(ctx, entry, member.data); err != nil {
			_ = zw.Close()
			return fmt.Errorf("diagnostics: write archive member: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("diagnostics: finish archive: %w", err)
	}
	return nil
}

func writeWithContext(ctx context.Context, dst io.Writer, data []byte) error {
	const chunkSize = 64 << 10
	for len(data) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := min(len(data), chunkSize)
		n, err := dst.Write(data[:chunk])
		if err != nil {
			return err
		}
		if n != chunk {
			return io.ErrShortWrite
		}
		data = data[chunk:]
	}
	return nil
}

func checkMembers(members []archiveMember) error {
	total := 0
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		if len(member.data) > MaxMemberBytes {
			return ErrLimit
		}
		if _, exists := seen[member.name]; exists {
			return errors.New("diagnostics: duplicate archive member")
		}
		seen[member.name] = struct{}{}
		total += len(member.data)
		if total > MaxTotalBytes {
			return ErrLimit
		}
	}
	return nil
}

func manifestMember(member archiveMember) ManifestMember {
	sum := sha256.Sum256(member.data)
	return ManifestMember{Name: member.name, Size: len(member.data), SHA256: hex.EncodeToString(sum[:])}
}

func checksumFile(members []archiveMember) []byte {
	var out strings.Builder
	for _, member := range members {
		sum := sha256.Sum256(member.data)
		fmt.Fprintf(&out, "%x  %s\n", sum, member.name)
	}
	return []byte(out.String())
}

func marshalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("diagnostics: encode member: %w", err)
	}
	return append(data, '\n'), nil
}

func marshalEvents(ctx context.Context, events []EventSnapshot, r *redactor, p *pseudonymizer) ([]byte, error) {
	var out strings.Builder
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		public := publicEvent{
			At: utc(event.At), Category: r.text(event.Category), Severity: r.text(event.Severity),
			DeviceID: p.alias(event.DeviceIdentifier, "device"), Message: r.text(event.Message),
		}
		data, err := json.Marshal(public)
		if err != nil {
			return nil, fmt.Errorf("diagnostics: encode event: %w", err)
		}
		if len(data) > MaxFreeTextBytes {
			public.Message = "[omitted: event exceeded limit]"
			data, _ = json.Marshal(public)
		}
		out.Write(data)
		out.WriteByte('\n')
	}
	return []byte(out.String()), nil
}

func sanitizeLogTail(ctx context.Context, input []byte, r *redactor) ([]byte, error) {
	if len(input) > MaxControllerLogBytes {
		input = input[len(input)-MaxControllerLogBytes:]
		if newline := strings.IndexByte(string(input), '\n'); newline >= 0 {
			input = input[newline+1:]
		}
	}
	lines := strings.Split(string(input), "\n")
	var out strings.Builder
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if line == "" {
			continue
		}
		if len(line) > MaxFreeTextBytes {
			line = `{"message":"[omitted: log record exceeded limit]"}`
		} else {
			line = sanitizeJSONLine(line, r)
		}
		if out.Len()+len(line)+1 > MaxControllerLogBytes {
			break
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return []byte(out.String()), nil
}

func sanitizeJSONLine(line string, r *redactor) string {
	var value any
	if json.Unmarshal([]byte(line), &value) != nil {
		encoded, _ := json.Marshal(map[string]string{"message": r.text(line)})
		return string(encoded)
	}
	value = r.jsonValue(value, "", 0)
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaxFreeTextBytes {
		return `{"message":"[omitted: log record exceeded limit]"}`
	}
	return string(encoded)
}

func buildREADME(in Input, controller ControllerSnapshot, devices []publicDevice, sources []publicSource, r *redactor) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "oonfeeWRT diagnostics bundle\n\nFormat version: %d\nBundle ID: %s\nGenerated: %s\nEvidence: stored controller snapshots only; zero live router calls.\n\n", FormatVersion, r.pseudo.bundleID, utc(in.GeneratedAt).Format(time.RFC3339))
	out.WriteString("Included: controller metadata, stored device facts, source coverage, redacted events, and the bounded controller log tail.\n")
	out.WriteString("Excluded: databases, keyrings, passphrases, password hashes, device credentials, private keys, session/CSRF tokens, WLAN/mesh keys, TOTP secrets, recovery codes, and raw secret-bearing UCI.\n")
	out.WriteString("Pre-existing Docker or service-manager logs outside the controller log sink are unavailable.\n\n")
	fmt.Fprintf(&out, "Controller: version %s; schema %d; platform %s; health %s; collected %s.\n", controller.Version, controller.Schema, controller.Platform, controller.Health, utc(controller.CollectedAt).Format(time.RFC3339))
	fmt.Fprintf(&out, "Devices: %d; sources: %d; events: %d.\n", len(devices), len(sources), len(in.Events))
	listed := min(len(devices), 32)
	for _, device := range devices[:listed] {
		fmt.Fprintf(&out, "- %s: %s; target %s; firmware %s; last observed %s.\n", device.ID, device.Model, device.Target, device.Firmware, utc(device.LastObservedAt).Format(time.RFC3339))
		for _, gap := range device.Gaps {
			fmt.Fprintf(&out, "  gap: %s\n", gap)
		}
	}
	if len(devices) > listed {
		fmt.Fprintf(&out, "- %d additional devices are listed in devices/devices.json.\n", len(devices)-listed)
	}
	for _, gap := range controller.Gaps {
		fmt.Fprintf(&out, "Controller gap: %s\n", gap)
	}
	out.WriteString("\nMembers:\n- README.txt\n- manifest.json\n- checksums.sha256\n- controller/metadata.json\n- devices/devices.json\n- coverage/sources.json\n- events/events.jsonl\n- logs/controller.jsonl\n")
	return []byte(out.String())
}

func redactStrings(ctx context.Context, r *redactor, values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if ctx.Err() != nil {
			return out
		}
		out = append(out, r.text(value))
	}
	return out
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
}

func utc(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC()
}

func (c *ControllerSnapshot) GeneratedUTC() {
	c.CollectedAt = utc(c.CollectedAt)
}

type pseudonymizer struct {
	salt         []byte
	bundleID     string
	aliases      map[string]string
	tokenAliases map[string]string
	replacer     *strings.Replacer
}

func newPseudonymizer(ctx context.Context, in Input) (*pseudonymizer, error) {
	salt := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("diagnostics: initialize pseudonyms: %w", err)
	}
	p := &pseudonymizer{salt: salt, aliases: make(map[string]string)}
	p.bundleID = "bundle-" + p.digest("bundle", hex.EncodeToString(salt))
	for _, device := range in.Devices {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.bind("device", device.Identifier, device.Name)
	}
	for _, identifier := range in.Identifiers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.register(identifier.Value, identifier.Kind)
	}
	for _, source := range in.Sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.register(source.DeviceIdentifier, "device")
	}
	for _, event := range in.Events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.register(event.DeviceIdentifier, "device")
	}
	p.rebuild()
	return p, nil
}

func (p *pseudonymizer) register(raw, kind string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if alias, ok := p.aliases[raw]; ok {
		return alias
	}
	prefix := safeKind(kind)
	if len(p.aliases) >= MaxPseudonyms {
		return prefix + "-redacted"
	}
	alias := prefix + "-" + p.digest(prefix, strings.ToLower(raw))
	p.aliases[raw] = alias
	return alias
}

func (p *pseudonymizer) bind(kind string, values ...string) {
	primary := firstNonempty(values...)
	alias := p.register(primary, kind)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := p.aliases[value]; !exists && len(p.aliases) < MaxPseudonyms {
			p.aliases[value] = alias
		}
	}
}

func (p *pseudonymizer) alias(raw, kind string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	if alias, ok := p.aliases[raw]; ok {
		return alias
	}
	return p.register(raw, kind)
}

func (p *pseudonymizer) digest(kind, raw string) string {
	h := hmac.New(sha256.New, p.salt)
	_, _ = io.WriteString(h, kind)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, raw)
	return hex.EncodeToString(h.Sum(nil)[:6])
}

func (p *pseudonymizer) rebuild() {
	keys := make([]string, 0, len(p.aliases))
	p.tokenAliases = make(map[string]string)
	for raw := range p.aliases {
		if identifierToken.MatchString(raw) {
			p.tokenAliases[raw] = p.aliases[raw]
		} else {
			keys = append(keys, raw)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j]) || len(keys[i]) == len(keys[j]) && keys[i] < keys[j]
	})
	pairs := make([]string, 0, len(keys)*2)
	for _, raw := range keys {
		pairs = append(pairs, raw, p.aliases[raw])
	}
	p.replacer = strings.NewReplacer(pairs...)
}

func (p *pseudonymizer) replace(value string) string {
	if p.replacer != nil {
		value = p.replacer.Replace(value)
	}
	return identifierTokens.ReplaceAllStringFunc(value, func(token string) string {
		if alias, ok := p.tokenAliases[token]; ok {
			return alias
		}
		return token
	})
}

func safeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "client", "device", "address", "account":
		return strings.ToLower(strings.TrimSpace(kind))
	default:
		return "identifier"
	}
}

var (
	secretQuoted     = regexp.MustCompile(`(?i)(["']?(?:password|passwd|passphrase|wpa_passphrase|psk|api[_ .-]?key|wlan[_ .-]?key|mesh[_ .-]?key|private[_ .-]?key|secret|token|authorization|cookie|csrf|totp|recovery[_ .-]?code|credential|\.key)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*')`)
	secretUnquoted   = regexp.MustCompile(`(?im)(["']?(?:password|passwd|passphrase|wpa_passphrase|psk|api[_ .-]?key|wlan[_ .-]?key|mesh[_ .-]?key|private[_ .-]?key|secret|token|authorization|cookie|csrf|totp|recovery[_ .-]?code|credential|\.key)["']?\s*[:=]\s*)[^\r\n]+`)
	authHeader       = regexp.MustCompile(`(?im)(\bauthorization\s*[:=]\s*)[^\r\n,;}]+`)
	bearerToken      = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	passwordHash     = regexp.MustCompile(`(?i)\$[a-z0-9][a-z0-9-]{0,31}\$[^\s"']*`)
	jwtToken         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	privateKeyBlock  = regexp.MustCompile(`(?is)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?(?:-----END [^-\r\n]*PRIVATE KEY-----|$)`)
	macAddress       = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)
	ipv4Address      = regexp.MustCompile(`\b(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}\b`)
	ipv6Candidate    = regexp.MustCompile(`(?i)(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}`)
	identifierToken  = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	identifierTokens = regexp.MustCompile(`[A-Za-z0-9_]+`)
	sensitiveKey     = regexp.MustCompile(`(?i)(?:password|passwd|passphrase|psk|api.?key|private.?key|secret|token|authorization|cookie|csrf|totp|recovery.?code|credential|wlan.?key|mesh.?key)`)
)

type redactor struct {
	pseudo  *pseudonymizer
	secrets *strings.Replacer
}

func newRedactor(p *pseudonymizer, secrets []string) *redactor {
	values := append([]string(nil), secrets...)
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pairs := make([]string, 0, len(values)*2)
	seen := make(map[string]struct{}, len(values))
	for _, secret := range values {
		if secret == "" {
			continue
		}
		if _, ok := seen[secret]; ok {
			continue
		}
		seen[secret] = struct{}{}
		pairs = append(pairs, secret, "[redacted]")
	}
	var replacer *strings.Replacer
	if len(pairs) != 0 {
		replacer = strings.NewReplacer(pairs...)
	}
	return &redactor{pseudo: p, secrets: replacer}
}

func (r *redactor) text(value string) string {
	value = r.secretText(value)
	value = r.pseudo.replace(value)
	return r.addresses(value)
}

// metadata keeps structured hardware and software facts intact even when an
// operator gave a device the same name. Secrets and addresses remain redacted.
func (r *redactor) metadata(value string) string {
	return r.addresses(r.secretText(value))
}

func (r *redactor) secretText(value string) string {
	if len(value) > MaxFreeTextBytes {
		value = value[:MaxFreeTextBytes]
	}
	value = privateKeyBlock.ReplaceAllString(value, "[redacted private key]")
	if r.secrets != nil {
		value = r.secrets.Replace(value)
	}
	value = authHeader.ReplaceAllString(value, "${1}[redacted]")
	value = secretQuoted.ReplaceAllString(value, "${1}[redacted]")
	value = secretUnquoted.ReplaceAllString(value, "${1}[redacted]")
	value = bearerToken.ReplaceAllString(value, "Bearer [redacted]")
	value = passwordHash.ReplaceAllString(value, "[redacted password hash]")
	value = jwtToken.ReplaceAllString(value, "[redacted token]")
	return value
}

func (r *redactor) addresses(value string) string {
	value = macAddress.ReplaceAllStringFunc(value, func(raw string) string { return r.pseudo.alias(strings.ToLower(raw), "address") })
	value = ipv4Address.ReplaceAllStringFunc(value, func(raw string) string { return r.pseudo.alias(raw, "address") })
	value = ipv6Candidate.ReplaceAllStringFunc(value, func(raw string) string {
		if net.ParseIP(raw) == nil {
			return raw
		}
		return r.pseudo.alias(strings.ToLower(raw), "address")
	})
	return value
}

func (r *redactor) jsonValue(value any, key string, depth int) any {
	if depth > 16 {
		return "[omitted: nesting limit]"
	}
	if key != "" && sensitiveKey.MatchString(key) {
		return "[redacted]"
	}
	switch typed := value.(type) {
	case string:
		return r.text(typed)
	case []any:
		if len(typed) > 256 {
			typed = typed[:256]
		}
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = r.jsonValue(typed[i], "", depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, min(len(typed), 256))
		count := 0
		for field, child := range typed {
			if count == 256 {
				break
			}
			safeField := r.text(field)
			out[safeField] = r.jsonValue(child, field, depth+1)
			count++
		}
		return out
	default:
		return value
	}
}
