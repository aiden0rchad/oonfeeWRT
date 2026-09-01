// Command releasecontainersmoke exercises the portable backup/restore API from
// inside the controller container. It uses only loopback and the Go standard
// library; no router or public network is reachable in the release gate.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxConfigBytes              = 16 << 10
	maxJSONBytes                = 4 << 20
	maxArtifactBytes            = 64 << 20
	backupPlanID                = "controller-backup-export-v1"
	restoreConfirmationContract = "controller-restore-confirm-v1"
	restoreTypedConfirmation    = "RESTORE CONTROLLER"
	restoreMediaType            = "application/vnd.oonfeewrt.backup"
)

type secretText []byte

func (s *secretText) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("secret must be a JSON string")
	}
	if len(value) < 16 || len(value) > 1024 {
		return errors.New("secret length is outside the smoke-test bounds")
	}
	clear(*s)
	*s = append((*s)[:0], value...)
	return nil
}

type config struct {
	BaseURL           string     `json:"base_url"`
	OwnerUsername     string     `json:"owner_username"`
	OwnerPassword     secretText `json:"owner_password"`
	ViewerUsername    string     `json:"viewer_username"`
	ViewerPassword    secretText `json:"viewer_password"`
	ExportPassphrase  secretText `json:"export_passphrase"`
	RuntimePassphrase secretText `json:"runtime_passphrase"`
}

func (c *config) clear() {
	clear(c.OwnerPassword)
	clear(c.ViewerPassword)
	clear(c.ExportPassphrase)
	clear(c.RuntimePassphrase)
	c.OwnerPassword = nil
	c.ViewerPassword = nil
	c.ExportPassphrase = nil
	c.RuntimePassphrase = nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: releasecontainersmoke <private-config-file>")
		os.Exit(2)
	}
	cfg, err := loadConfig(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "release container smoke:", err)
		os.Exit(1)
	}
	defer cfg.clear()
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(root, 25*time.Minute)
	defer cancel()
	if err := runSmoke(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "release container smoke:", err)
		os.Exit(1)
	}
	fmt.Println("release container smoke: clean schema-20 export/restore passed with zero devices")
}

func loadConfig(path string) (*config, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("open private config")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("config must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open private config")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		clear(data)
		return nil, errors.New("read bounded private config")
	}
	defer clear(data)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg config
	if err := dec.Decode(&cfg); err != nil {
		cfg.clear()
		return nil, errors.New("decode private config")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		cfg.clear()
		return nil, errors.New("private config has trailing data")
	}
	if err := cfg.validate(); err != nil {
		cfg.clear()
		return nil, err
	}
	return &cfg, nil
}

func (c *config) validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil ||
		u.Path != "/api/v1" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base_url must be direct IPv4 loopback HTTP ending in /api/v1")
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port < 1024 || port == 8080 {
		return errors.New("base_url must use an explicit non-8080 unprivileged port")
	}
	if !validUsername(c.OwnerUsername) || !validUsername(c.ViewerUsername) || c.OwnerUsername == c.ViewerUsername {
		return errors.New("config usernames are invalid")
	}
	if len(c.OwnerPassword) == 0 || len(c.ViewerPassword) == 0 || len(c.ExportPassphrase) == 0 || len(c.RuntimePassphrase) == 0 {
		return errors.New("config secrets are incomplete")
	}
	return nil
}

func validUsername(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for i := range value {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

type apiClient struct {
	base     string
	root     string
	http     *http.Client
	csrf     string
	instance string
}

func newAPIClient(base string) (*apiClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &apiClient{
		base: strings.TrimSuffix(base, "/"),
		root: strings.TrimSuffix(base, "/api/v1"),
		http: &http.Client{
			Transport: transport,
			Jar:       jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *apiClient) resetSession() error {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	c.http.Jar = jar
	c.csrf = ""
	return nil
}

func (c *apiClient) requestJSON(ctx context.Context, method, path string, input, output any, expected int) error {
	var payload []byte
	var body io.Reader
	if input != nil {
		var err error
		payload, err = json.Marshal(input)
		if err != nil {
			return errors.New("encode request")
		}
		defer clear(payload)
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return errors.New("create request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "oonfeewrt-release-smoke/1")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" && method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-Oonfee-CSRF", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s failed", method, path)
	}
	defer resp.Body.Close()
	data, err := readBounded(resp.Body, maxJSONBytes)
	if err != nil {
		return fmt.Errorf("%s %s returned an invalid bounded response", method, path)
	}
	defer clear(data)
	if resp.StatusCode != expected {
		var coded struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &coded)
		if !validErrorCode(coded.Code) {
			coded.Code = "unknown"
		}
		return fmt.Errorf("%s %s returned HTTP %d (%s)", method, path, resp.StatusCode, coded.Code)
	}
	instance := resp.Header.Get("X-OonfeeWRT-Instance")
	if !validID(instance) {
		return fmt.Errorf("%s %s returned an invalid controller instance", method, path)
	}
	c.instance = instance
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("%s %s returned invalid JSON", method, path)
		}
	}
	return nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(data)) > limit {
		clear(data)
		return nil, errors.New("response exceeds limit")
	}
	return data, nil
}

func (c *apiClient) waitHealth(ctx context.Context) error {
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(attempt, http.MethodGet, c.root+"/healthz", nil)
		resp, err := c.http.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
		}
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("controller did not become healthy")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (c *apiClient) waitSetupInstance(ctx context.Context, previous string) (bool, error) {
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		var state struct {
			NeedsSetup bool `json:"needs_setup"`
		}
		err := c.requestJSON(attempt, http.MethodGet, "/setup", nil, &state, http.StatusOK)
		cancel()
		if err == nil && c.instance != previous {
			return state.NeedsSetup, nil
		}
		select {
		case <-ctx.Done():
			return false, errors.New("controller did not reopen in a new instance")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type sessionResponse struct {
	CSRF string `json:"csrf"`
}

type account struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
}

type accountsResponse struct {
	Accounts []account `json:"accounts"`
}

type backupResponse struct {
	Descriptor struct {
		PlanID        string `json:"plan_id"`
		Format        string `json:"format"`
		FormatVersion int    `json:"format_version"`
		FileExtension string `json:"file_extension"`
		Encryption    string `json:"encryption"`
	} `json:"descriptor"`
	Disclosure struct {
		RouterManagementCalls       bool `json:"router_management_calls"`
		RouterChanges               bool `json:"router_changes"`
		AutomaticRouterApply        bool `json:"automatic_router_apply"`
		SeparateExportPassphrase    bool `json:"separate_export_passphrase"`
		ExportPassphraseRecoverable bool `json:"export_passphrase_recoverable"`
	} `json:"disclosure"`
	Limits struct {
		MaxArtifactBytes int64 `json:"max_artifact_bytes"`
	} `json:"limits"`
}

type backupJob struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type backupJobResponse struct {
	Job backupJob `json:"job"`
}

type restoreResponse struct {
	Descriptor struct {
		UploadContentType    string `json:"upload_content_type"`
		ConfirmationContract string `json:"confirmation_contract"`
		TypedConfirmation    string `json:"typed_confirmation"`
	} `json:"descriptor"`
	Disclosure struct {
		RouterManagementCalls bool `json:"router_management_calls"`
		RouterChanges         bool `json:"router_changes"`
		LiveControllerChanges bool `json:"live_controller_changes"`
		AutomaticRouterApply  bool `json:"automatic_router_apply"`
	} `json:"disclosure"`
}

type restorePreview struct {
	ID           string `json:"id"`
	UploadID     string `json:"upload_id"`
	State        string `json:"state"`
	ErrorCode    string `json:"error_code"`
	PlanID       string `json:"plan_id"`
	SourceSchema int    `json:"source_schema"`
	TargetSchema int    `json:"target_schema"`
	Counts       struct {
		Devices int `json:"devices"`
	} `json:"counts"`
}

type restorePreviewResponse struct {
	Preview restorePreview `json:"preview"`
}

func runSmoke(ctx context.Context, cfg *config) error {
	api, err := newAPIClient(cfg.BaseURL)
	if err != nil {
		return errors.New("initialize loopback client")
	}
	if err := api.waitHealth(ctx); err != nil {
		return err
	}
	var setupState struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	if err := api.requestJSON(ctx, http.MethodGet, "/setup", nil, &setupState, http.StatusOK); err != nil {
		return err
	}
	if !setupState.NeedsSetup {
		return errors.New("clean container did not require setup")
	}
	initialInstance := api.instance
	var session sessionResponse
	if err := api.requestJSON(ctx, http.MethodPost, "/setup", map[string]string{
		"username": cfg.OwnerUsername, "password": string(cfg.OwnerPassword),
	}, &session, http.StatusCreated); err != nil {
		return err
	}
	if session.CSRF == "" {
		return errors.New("setup returned no CSRF token")
	}
	api.csrf = session.CSRF
	if err := assertZeroDevices(ctx, api); err != nil {
		return err
	}
	if err := reauthenticate(ctx, api, cfg.OwnerPassword); err != nil {
		return err
	}

	var backups backupResponse
	if err := api.requestJSON(ctx, http.MethodGet, "/backups", nil, &backups, http.StatusOK); err != nil {
		return err
	}
	if backups.Descriptor.PlanID != backupPlanID || backups.Descriptor.Format != "oonfeewrt-portable-backup" ||
		backups.Descriptor.FormatVersion != 1 || backups.Descriptor.FileExtension != ".oowrtbak" ||
		backups.Descriptor.Encryption == "" || backups.Disclosure.RouterManagementCalls ||
		backups.Disclosure.RouterChanges || backups.Disclosure.AutomaticRouterApply ||
		!backups.Disclosure.SeparateExportPassphrase || backups.Disclosure.ExportPassphraseRecoverable {
		return errors.New("backup descriptor or disclosure contract changed")
	}
	var started backupJobResponse
	if err := api.requestJSON(ctx, http.MethodPost, "/backups", map[string]any{
		"plan_id": backups.Descriptor.PlanID, "acknowledge_sensitive_content": true,
		"export_passphrase":         string(cfg.ExportPassphrase),
		"confirm_export_passphrase": string(cfg.ExportPassphrase),
	}, &started, http.StatusAccepted); err != nil {
		return err
	}
	if !validID(started.Job.ID) {
		return errors.New("backup returned an invalid job id")
	}
	job, err := waitBackup(ctx, api, started.Job.ID)
	if err != nil {
		return err
	}
	if job.SizeBytes <= 0 || job.SizeBytes > backups.Limits.MaxArtifactBytes ||
		job.SizeBytes > maxArtifactBytes || !validHex(job.SHA256, sha256.Size*2) {
		return errors.New("completed backup returned invalid integrity metadata")
	}
	if err := reauthenticate(ctx, api, cfg.OwnerPassword); err != nil {
		return err
	}
	artifact, err := downloadBackup(ctx, api, job)
	if err != nil {
		return err
	}
	defer clear(artifact)

	var created struct {
		Account account `json:"account"`
	}
	if err := api.requestJSON(ctx, http.MethodPost, "/accounts", map[string]string{
		"username": cfg.ViewerUsername, "password": string(cfg.ViewerPassword), "role": "viewer",
	}, &created, http.StatusCreated); err != nil {
		return err
	}
	if created.Account.Username != cfg.ViewerUsername || created.Account.Role != "viewer" || !created.Account.Enabled {
		return errors.New("post-backup viewer account was not created")
	}
	if err := assertAccounts(ctx, api, cfg, false); err != nil {
		return err
	}
	if err := reauthenticate(ctx, api, cfg.OwnerPassword); err != nil {
		return err
	}

	var restores restoreResponse
	if err := api.requestJSON(ctx, http.MethodGet, "/restores", nil, &restores, http.StatusOK); err != nil {
		return err
	}
	if restores.Descriptor.UploadContentType != restoreMediaType ||
		restores.Descriptor.ConfirmationContract != restoreConfirmationContract ||
		restores.Descriptor.TypedConfirmation != restoreTypedConfirmation ||
		restores.Disclosure.RouterManagementCalls || restores.Disclosure.RouterChanges ||
		restores.Disclosure.LiveControllerChanges || restores.Disclosure.AutomaticRouterApply {
		return errors.New("restore descriptor or disclosure contract changed")
	}
	uploadID, err := uploadRestore(ctx, api, artifact, restores.Descriptor.UploadContentType)
	if err != nil {
		return err
	}
	var previewStarted restorePreviewResponse
	if err := api.requestJSON(ctx, http.MethodPost, "/restores/previews", map[string]string{
		"upload_id": uploadID, "export_passphrase": string(cfg.ExportPassphrase),
	}, &previewStarted, http.StatusAccepted); err != nil {
		return err
	}
	if !validID(previewStarted.Preview.ID) {
		return errors.New("restore returned an invalid preview id")
	}
	preview, err := waitPreview(ctx, api, previewStarted.Preview.ID, uploadID)
	if err != nil {
		return err
	}
	if !validRestorePlanID(preview.PlanID) || preview.SourceSchema != 20 || preview.TargetSchema != 20 || preview.Counts.Devices != 0 {
		return errors.New("restore preview did not bind the expected zero-device schema-20 plan")
	}
	if err := reauthenticate(ctx, api, cfg.OwnerPassword); err != nil {
		return err
	}
	var intent struct {
		Intent struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"intent"`
	}
	if err := api.requestJSON(ctx, http.MethodPost, "/restores/previews/"+preview.ID+"/confirm", map[string]any{
		"plan_id": preview.PlanID, "export_passphrase": string(cfg.ExportPassphrase),
		"destination_runtime_passphrase": string(cfg.RuntimePassphrase),
		"typed_confirmation":             restores.Descriptor.TypedConfirmation,
		"acknowledge_restart":            true, "acknowledge_session_revocation": true,
		"acknowledge_router_writes_suppressed": true, "acknowledge_no_automatic_router_apply": true,
	}, &intent, http.StatusAccepted); err != nil {
		return err
	}
	if intent.Intent.State != "accepted" || !validHex(intent.Intent.ID, 32) || strings.Trim(intent.Intent.ID, "0") == "" {
		return errors.New("restore confirmation returned an invalid intent")
	}
	if err := api.resetSession(); err != nil {
		return errors.New("reset restored session")
	}
	needsSetup, err := api.waitSetupInstance(ctx, initialInstance)
	if err != nil {
		return err
	}
	if needsSetup {
		return errors.New("restored controller lost its owner account")
	}
	session = sessionResponse{}
	if err := api.requestJSON(ctx, http.MethodPost, "/login", map[string]string{
		"username": cfg.OwnerUsername, "password": string(cfg.OwnerPassword),
	}, &session, http.StatusOK); err != nil {
		return err
	}
	if session.CSRF == "" {
		return errors.New("restored login returned no CSRF token")
	}
	api.csrf = session.CSRF
	if err := assertAccounts(ctx, api, cfg, true); err != nil {
		return err
	}
	var suppression struct {
		Suppression struct {
			Active    bool   `json:"active"`
			RestoreID string `json:"restore_id"`
		} `json:"suppression"`
	}
	if err := api.requestJSON(ctx, http.MethodGet, "/restores/suppression", nil, &suppression, http.StatusOK); err != nil {
		return err
	}
	if !suppression.Suppression.Active || suppression.Suppression.RestoreID != intent.Intent.ID {
		return errors.New("restored router-write suppression does not match the accepted intent")
	}
	if err := assertZeroDevices(ctx, api); err != nil {
		return err
	}
	return api.resetSession()
}

func reauthenticate(ctx context.Context, api *apiClient, password []byte) error {
	return api.requestJSON(ctx, http.MethodPost, "/session/reauth",
		map[string]string{"password": string(password)}, nil, http.StatusOK)
}

func assertZeroDevices(ctx context.Context, api *apiClient) error {
	var response struct {
		Devices []json.RawMessage `json:"devices"`
	}
	if err := api.requestJSON(ctx, http.MethodGet, "/devices", nil, &response, http.StatusOK); err != nil {
		return err
	}
	if len(response.Devices) != 0 {
		return errors.New("release smoke controller contains devices")
	}
	return nil
}

func assertAccounts(ctx context.Context, api *apiClient, cfg *config, restored bool) error {
	var response accountsResponse
	if err := api.requestJSON(ctx, http.MethodGet, "/accounts", nil, &response, http.StatusOK); err != nil {
		return err
	}
	owner, viewer := false, false
	for _, current := range response.Accounts {
		owner = owner || current.Username == cfg.OwnerUsername && current.Role == "owner" && current.Enabled
		viewer = viewer || current.Username == cfg.ViewerUsername
	}
	if !owner || (!restored && (!viewer || len(response.Accounts) != 2)) || (restored && (viewer || len(response.Accounts) != 1)) {
		return errors.New("account rollback invariant failed")
	}
	return nil
}

func waitBackup(ctx context.Context, api *apiClient, id string) (backupJob, error) {
	deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		var response backupJobResponse
		if err := api.requestJSON(deadline, http.MethodGet, "/backups/"+id, nil, &response, http.StatusOK); err != nil {
			return backupJob{}, err
		}
		if response.Job.ID != id {
			return backupJob{}, errors.New("backup job identity changed")
		}
		switch response.Job.State {
		case "completed":
			return response.Job, nil
		case "failed", "cancelled":
			return backupJob{}, errors.New("backup job did not complete")
		}
		if err := wait(deadline); err != nil {
			return backupJob{}, errors.New("backup job timed out")
		}
	}
}

func downloadBackup(ctx context.Context, api *apiClient, job backupJob) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api.base+"/backups/"+job.ID+"/download", nil)
	if err != nil {
		return nil, errors.New("create backup download")
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := api.http.Do(req)
	if err != nil {
		return nil, errors.New("download backup")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != job.SizeBytes {
		return nil, errors.New("backup download failed integrity precheck")
	}
	artifact, err := readBounded(resp.Body, job.SizeBytes)
	if err != nil || int64(len(artifact)) != job.SizeBytes {
		clear(artifact)
		return nil, errors.New("backup download length mismatch")
	}
	digest := sha256.Sum256(artifact)
	if hex.EncodeToString(digest[:]) != job.SHA256 {
		clear(artifact)
		return nil, errors.New("backup download checksum mismatch")
	}
	return artifact, nil
}

func uploadRestore(ctx context.Context, api *apiClient, artifact []byte, mediaType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.base+"/restores/uploads", bytes.NewReader(artifact))
	if err != nil {
		return "", errors.New("create restore upload")
	}
	req.Header.Set("Content-Type", mediaType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Oonfee-CSRF", api.csrf)
	resp, err := api.http.Do(req)
	if err != nil {
		return "", errors.New("upload restore")
	}
	defer resp.Body.Close()
	data, err := readBounded(resp.Body, maxJSONBytes)
	if err != nil {
		return "", errors.New("restore upload returned an invalid bounded response")
	}
	defer clear(data)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("restore upload returned HTTP %d", resp.StatusCode)
	}
	if instance := resp.Header.Get("X-OonfeeWRT-Instance"); !validID(instance) || instance != api.instance {
		return "", errors.New("restore upload crossed controller instances")
	}
	var response struct {
		Upload struct {
			ID string `json:"id"`
		} `json:"upload"`
	}
	if json.Unmarshal(data, &response) != nil || !validID(response.Upload.ID) {
		return "", errors.New("restore upload returned an invalid id")
	}
	return response.Upload.ID, nil
}

func waitPreview(ctx context.Context, api *apiClient, id, uploadID string) (restorePreview, error) {
	deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		var response restorePreviewResponse
		if err := api.requestJSON(deadline, http.MethodGet, "/restores/previews/"+id, nil, &response, http.StatusOK); err != nil {
			return restorePreview{}, err
		}
		if response.Preview.ID != id || response.Preview.UploadID != uploadID {
			return restorePreview{}, errors.New("restore preview identity changed")
		}
		switch response.Preview.State {
		case "completed":
			return response.Preview, nil
		case "failed", "cancelled":
			if validErrorCode(response.Preview.ErrorCode) {
				return restorePreview{}, fmt.Errorf("restore preview did not complete (%s)", response.Preview.ErrorCode)
			}
			return restorePreview{}, errors.New("restore preview did not complete")
		}
		if err := wait(deadline); err != nil {
			return restorePreview{}, errors.New("restore preview timed out")
		}
	}
}

func validRestorePlanID(value string) bool {
	prefix := restoreConfirmationContract + "."
	return strings.HasPrefix(value, prefix) && validHex(strings.TrimPrefix(value, prefix), sha256.Size*2)
}

func validErrorCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if c != '_' && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(250 * time.Millisecond):
		return nil
	}
}

func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c != '-' && c != '_' && (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func validHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
