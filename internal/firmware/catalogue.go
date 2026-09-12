// Package firmware checks official OpenWrt release metadata. It cannot upload,
// install, or execute an image and never treats a catalogue checksum as proof
// that image bytes or the device's upgrade path have been verified.
package firmware

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const SourceURL = "https://downloads.openwrt.org"
const maxManifestBytes = 8 << 20

type Identity struct {
	BoardName  string `json:"board_name"`
	Target     string `json:"target"`
	RootFSType string `json:"rootfs_type"`
	Release    string `json:"release"`
}

type Image struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Filesystem string `json:"filesystem"`
}

type Result struct {
	State          string   `json:"state"`
	CurrentVersion string   `json:"current_version"`
	LatestVersion  string   `json:"latest_version,omitempty"`
	Branch         string   `json:"branch,omitempty"`
	CheckedAt      int64    `json:"checked_at"`
	SourceURL      string   `json:"source_url"`
	Message        string   `json:"message"`
	Image          *Image   `json:"image,omitempty"`
	Limitations    []string `json:"limitations"`
}

// Checker is bounded, uses only a fixed HTTPS origin, rejects redirects, and
// does not cache successes across failed checks. No router credential is needed.
type Checker struct {
	client *http.Client
	slots  chan struct{}
	now    func() time.Time
}

func NewChecker() *Checker {
	return &Checker{
		client: &http.Client{
			Timeout: 12 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return fmt.Errorf("firmware catalogue redirects are not followed")
			},
		},
		slots: make(chan struct{}, 4),
		now:   time.Now,
	}
}

var versionPattern = regexp.MustCompile(`^([0-9]{2}\.[0-9]{2})\.([0-9]{1,6})$`)
var targetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*/[a-z0-9][a-z0-9_-]*$`)
var imageNamePattern = regexp.MustCompile(`^openwrt-[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func (c *Checker) Check(ctx context.Context, identity Identity) Result {
	r := Result{
		State: "unsupported", CheckedAt: c.now().UnixMilli(), SourceURL: SourceURL + "/.versions.json",
		Limitations: []string{
			"Board identity comes from the stored capability probe. Re-probe after changing router firmware.",
			"This checks release metadata, not downloaded image bytes or on-device sysupgrade compatibility.",
			"Only maintenance updates within the current stable or oldstable release branch are considered; major-version upgrades require a separate migration review.",
			"Custom packages, configuration compatibility, free memory, backups, and recovery access still require review before installation.",
		},
	}
	parts := strings.Fields(identity.Release)
	if len(parts) < 2 || parts[0] != "OpenWrt" || !versionPattern.MatchString(parts[1]) {
		r.Message = "An official stable OpenWrt version could not be identified. Snapshots and custom distributions need their own upgrade guidance."
		return r
	}
	r.CurrentVersion = parts[1]
	current := versionPattern.FindStringSubmatch(parts[1])
	r.Branch = current[1]
	if identity.BoardName == "" || len(identity.BoardName) > 256 || !targetPattern.MatchString(identity.Target) || identity.RootFSType == "" {
		r.Message = "The stored probe does not contain a usable board name, target, and root filesystem. Re-probe the device before checking firmware."
		return r
	}
	if slices.Contains([]string{"x86", "armsr", "loongarch"}, strings.SplitN(identity.Target, "/", 2)[0]) {
		r.Message = "This target requires boot-mode and disk-layout evidence that the current probe does not capture. Select its image through OpenWrt's firmware selector."
		return r
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		r.State, r.Message = "error", "Other firmware checks are still running. Try again shortly."
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var versions struct {
		Stable    string `json:"stable_version"`
		OldStable string `json:"oldstable_version"`
	}
	if err := c.readJSON(ctx, "/.versions.json", &versions); err != nil {
		r.State, r.Message = "error", "The official release catalogue could not be checked. Check controller internet access and try again; this is not an up-to-date result."
		return r
	}
	if !versionPattern.MatchString(versions.Stable) || (versions.OldStable != "" && !versionPattern.MatchString(versions.OldStable)) {
		r.State, r.Message = "error", "The official release catalogue did not contain valid stable-version metadata. No up-to-date status has been confirmed."
		return r
	}
	for _, version := range []string{versions.Stable, versions.OldStable} {
		match := versionPattern.FindStringSubmatch(version)
		if match != nil && match[1] == r.Branch {
			if r.LatestVersion != "" && r.LatestVersion != version {
				r.State, r.Message = "error", "The release catalogue returned conflicting versions for this branch. No image was selected."
				return r
			}
			r.LatestVersion = version
		}
	}
	if r.LatestVersion == "" {
		r.Message = "This release branch is not listed as stable or oldstable. Review OpenWrt's release and migration guidance; no cross-branch image was selected."
		return r
	}
	directory := "/releases/" + r.LatestVersion + "/targets/" + identity.Target + "/"
	r.SourceURL = SourceURL + directory + "profiles.json"
	var manifest struct {
		Target   string `json:"target"`
		Version  string `json:"version_number"`
		Profiles map[string]struct {
			Supported []string `json:"supported_devices"`
			Images    []struct {
				Name       string `json:"name"`
				Type       string `json:"type"`
				Filesystem string `json:"filesystem"`
				SHA256     string `json:"sha256"`
				Size       int64  `json:"size"`
			} `json:"images"`
		} `json:"profiles"`
	}
	if err := c.readJSON(ctx, directory+"profiles.json", &manifest); err != nil {
		r.State, r.Message = "error", "The target's official image catalogue could not be checked. No matching image or up-to-date status has been confirmed."
		return r
	}
	if manifest.Target != identity.Target || manifest.Version != r.LatestVersion {
		r.State, r.Message = "error", "The image catalogue does not match the requested target and release. No image was selected."
		return r
	}
	var candidates []Image
	matchedProfiles := 0
	for _, profile := range manifest.Profiles {
		if !slices.Contains(profile.Supported, identity.BoardName) {
			continue
		}
		matchedProfiles++
		for _, image := range profile.Images {
			if image.Type != "sysupgrade" || image.Filesystem != identity.RootFSType {
				continue
			}
			checksum, err := hex.DecodeString(image.SHA256)
			prefix := "openwrt-" + r.LatestVersion + "-" + strings.ReplaceAll(identity.Target, "/", "-") + "-"
			if err != nil || len(checksum) != 32 || image.Size <= 0 || image.Size > 2<<30 || !imageNamePattern.MatchString(image.Name) || !strings.HasPrefix(image.Name, prefix) {
				r.State, r.Message = "error", "The matching image has incomplete or invalid filename, size, or checksum metadata. No image was selected."
				return r
			}
			candidates = append(candidates, Image{
				Name: image.Name, URL: SourceURL + directory + image.Name, SHA256: strings.ToLower(image.SHA256), Size: image.Size, Filesystem: image.Filesystem,
			})
		}
	}
	if matchedProfiles != 1 || len(candidates) != 1 {
		r.Message = "The catalogue does not identify exactly one sysupgrade image for this board and filesystem. No image was guessed; use OpenWrt's device-specific guidance."
		return r
	}
	r.Image = &candidates[0]
	installedPatch, _ := strconv.Atoi(current[2])
	latestPatch, _ := strconv.Atoi(versionPattern.FindStringSubmatch(r.LatestVersion)[2])
	switch {
	case latestPatch > installedPatch:
		r.State, r.Message = "available", "A newer maintenance release has matching image metadata. Review the device-specific upgrade procedure before installation."
	case latestPatch == installedPatch:
		r.State, r.Message = "current", "The reported firmware version matches the latest listed maintenance release in this branch. This is not a package-security or custom-build assessment."
	default:
		r.State, r.Message = "ahead", "The reported firmware is newer than this catalogue lists. The catalogue may be behind; no downgrade is recommended."
	}
	return r
}

func (c *Checker) readJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SourceURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "oonfeeWRT-firmware-catalogue/1")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("catalogue status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxManifestBytes {
		return fmt.Errorf("catalogue exceeds size limit")
	}
	return json.Unmarshal(body, out)
}
