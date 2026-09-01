package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseBuildContract(t *testing.T) {
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"ARG TARGETOS TARGETARCH VERSION=dev",
		"go build -trimpath -buildvcs=false",
		"-buildid= -X main.version=$VERSION",
		"node:22-alpine@sha256:",
		"golang:1.26.6-alpine@sha256:",
		`LABEL org.opencontainers.image.licenses="Apache-2.0"`,
		"COPY LICENSE NOTICE THIRD_PARTY_LICENSES /licenses/",
		"COPY RELEASE-NOTES.md /release/",
		"COPY deploy/docker-compose.yml /release/docker-compose.yml",
		`HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/oonfeewrtd", "-healthcheck"]`,
	} {
		if !strings.Contains(string(dockerfile), required) {
			t.Errorf("Dockerfile lost release build contract %q", required)
		}
	}

	ignored, err := os.ReadFile("../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]bool{}
	for _, line := range strings.Split(string(ignored), "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	for _, required := range []string{
		".git/", ".run/", "data/", "**/passphrase", "**/keyring.json", "**/*.db", "**/*.db-*", "**/*.key", "**/*.pem",
		".env", ".env.*", "**/.env", "**/.env.*", "**/node_modules/", "ui/dist/", "dist/", "*.oowrtbak",
		"oonfeewrt-diagnostics-*.zip", "**/.oonfeewrt-recovery/", "**/.oonfeewrt-backup-*.db.tmp",
		"*-before-factory-reset.tar*",
	} {
		if !lines[required] {
			t.Errorf(".dockerignore does not exclude %q", required)
		}
	}
	gitignore, err := os.ReadFile("../.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"keyring.json", "*.db", "*.db-*", ".env", ".env.*", "/dist/", "*.oowrtbak",
		"oonfeewrt-diagnostics-*.zip", "**/.oonfeewrt-recovery/", ".oonfeewrt-backup-*.db.tmp",
	} {
		if !strings.Contains(string(gitignore), secret+"\n") {
			t.Errorf(".gitignore does not exclude %q", secret)
		}
	}

	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"ui:", "build: ui", "test: ui", "check: ui", "image:", "release:", "release-check:"} {
		if !strings.Contains(string(makefile), target) {
			t.Errorf("Makefile lost documented target %q", target)
		}
	}
	for _, script := range []string{
		"../tools/release-build.sh", "../tools/reproducible-build-check.sh", "../tools/generate-third-party-licenses.py",
	} {
		info, err := os.Stat(script)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", script)
		}
	}
	licenseGenerator, err := os.ReadFile("../tools/generate-third-party-licenses.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"GOROOT"`, `"GOVERSION"`, "GO_LICENSE_SHA256", "VETTED_SHA256",
		"ca-certificates-bundle", "20260611-r0",
	} {
		if !strings.Contains(string(licenseGenerator), required) {
			t.Errorf("license generator lost pinned runtime inventory %q", required)
		}
	}
	reproScript, err := os.ReadFile("../tools/reproducible-build-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"*.oowrtbak", "oonfeewrt-diagnostics-*.zip", "**/.oonfeewrt-recovery/",
		`SOURCE_DATE_EPOCH="$epoch" tools/release-build.sh "$version" "$tmp/first"`,
		`SOURCE_DATE_EPOCH="$epoch" tools/release-build.sh "$version" "$tmp/second"`,
		`cmp -s "$tmp/first/SHA256SUMS" "$tmp/second/SHA256SUMS"`,
		`gzip -t "$tmp/first/$archive" "$tmp/second/$archive"`,
		"sha256sum -c SHA256SUMS", "shasum -a 256 -c SHA256SUMS",
		"oonfeewrt-recoverycheck", `actual=$("$controller" -version)`,
	} {
		if !strings.Contains(string(reproScript), required) {
			t.Errorf("reproducible release gate lost %q", required)
		}
	}
	if _, err := os.Stat("../ui/dist/.gitkeep"); err != nil {
		t.Fatalf("tracked UI embed placeholder is unavailable: %v", err)
	}
	packageJSON, err := os.ReadFile("../ui/package.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packageJSON), "writeFileSync('dist/.gitkeep','')") {
		t.Error("UI build no longer restores the clean-clone embed placeholder")
	}
	releaseScript, err := os.ReadFile("../tools/release-build.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, packaged := range []string{
		"THIRD_PARTY_LICENSES", `cp RELEASE-NOTES.md "$stage/RELEASE-NOTES.md"`,
		"deploy/docker-compose.yml",
		"generate-third-party-licenses.py --check",
	} {
		if !strings.Contains(string(releaseScript), packaged) {
			t.Errorf("release archives lost %q", packaged)
		}
	}
	for _, artifact := range []string{"../THIRD_PARTY_LICENSES", "../RELEASE-NOTES.md"} {
		info, err := os.Stat(artifact)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Errorf("release artifact %s is empty", artifact)
		}
	}
	thirdPartyLicenses, err := os.ReadFile("../THIRD_PARTY_LICENSES")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Go toolchain/runtime and standard library: go1.26.6",
		"Runtime CA roots: Alpine ca-certificates-bundle 20260611-r0",
		"License expression: MPL-2.0 AND MIT",
		"Mozilla Public License Version 2.0",
		"Copyright (c) 2013-2014 Timo Teräs",
	} {
		if !strings.Contains(string(thirdPartyLicenses), required) {
			t.Errorf("third-party license artifact lost %q", required)
		}
	}

	compose, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, hardening := range []string{
		"read_only: true", "/tmp:rw,noexec,nosuid,nodev", "cap_drop:",
		"- ALL", "no-new-privileges:true", "create_host_path: false",
		`image: "ghcr.io/aiden0rchad/oonfeewrt:${OONFEE_VERSION:?set OONFEE_VERSION to a release tag}"`,
		`host_ip: "${OONFEE_HTTP_BIND:-127.0.0.1}"`,
		`OONFEE_LISTEN: ":8080"`,
	} {
		if !strings.Contains(string(compose), hardening) {
			t.Errorf("compose lost runtime hardening %q", hardening)
		}
	}
	for _, line := range strings.Split(string(compose), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "network_mode:") {
			t.Error("compose must keep host networking as a documented opt-in")
		}
	}
}

func TestComposeHTTPBindProfiles(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker CLI is unavailable")
	}
	if err := exec.Command(docker, "compose", "version").Run(); err != nil {
		t.Skipf("Docker Compose is unavailable: %v", err)
	}

	for _, tc := range []struct {
		name string
		bind string
		want string
	}{
		{name: "safe default", want: "127.0.0.1"},
		{name: "management LAN", bind: "192.0.2.44", want: "192.0.2.44"},
		{name: "explicit all interfaces", bind: "0.0.0.0", want: "0.0.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := renderComposeConfig(t, docker, tc.bind)
			service := config.Services["oonfeewrt"]
			if len(service.Ports) != 1 {
				t.Fatalf("ports = %+v, want one published HTTP port", service.Ports)
			}
			port := service.Ports[0]
			if port.HostIP != tc.want || port.Target != 8080 || port.Published != "8080" || port.Protocol != "tcp" {
				t.Fatalf("port = %+v, want host_ip=%q target=8080 published=8080 protocol=tcp", port, tc.want)
			}
			if service.Environment["OONFEE_LISTEN"] != ":8080" {
				t.Fatalf("OONFEE_LISTEN = %q, want :8080", service.Environment["OONFEE_LISTEN"])
			}
			if _, passed := service.Environment["OONFEE_HTTP_BIND"]; passed {
				t.Fatal("OONFEE_HTTP_BIND must remain host-side Compose interpolation")
			}
		})
	}

}

type composeConfig struct {
	Services map[string]struct {
		Environment map[string]string `json:"environment"`
		Ports       []struct {
			HostIP    string `json:"host_ip"`
			Target    int    `json:"target"`
			Published string `json:"published"`
			Protocol  string `json:"protocol"`
		} `json:"ports"`
	} `json:"services"`
}

func renderComposeConfig(t *testing.T, docker, bind string) composeConfig {
	t.Helper()
	output, err := composeConfigCommand(docker, bind).Output()
	if err != nil {
		t.Fatalf("render Compose config: %v", err)
	}
	var config composeConfig
	if err := json.Unmarshal(output, &config); err != nil {
		t.Fatalf("decode Compose config: %v", err)
	}
	return config
}

func composeConfigCommand(docker, bind string) *exec.Cmd {
	cmd := exec.Command(docker, "compose", "-f", "docker-compose.yml", "config", "--format", "json")
	cmd.Env = make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "OONFEE_VERSION=") || strings.HasPrefix(value, "OONFEE_HTTP_BIND=") {
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env, "OONFEE_VERSION=v0.0.0-test")
	if bind != "" {
		cmd.Env = append(cmd.Env, "OONFEE_HTTP_BIND="+bind)
	}
	return cmd
}

func TestReleaseTreeHasNoKnownLabFixtureIdentifiers(t *testing.T) {
	blocked := []string{
		"30:23:03" + ":db:be", "32:23:03" + ":db:be", "86:d8:1b" + ":c5:19",
		"60:38" + ":e0", "roland-" + "laptop", "oonfee-" + "roam",
		"wrt-" + "cleanroom", "oonfeewrt-" + "probe-5g",
	}
	for _, root := range []string{"../internal", "../tools", "../ui/src"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			switch filepath.Ext(path) {
			case ".go", ".py", ".ts", ".tsx":
			default:
				return nil
			}
			text := strings.ToLower(string(mustReadFile(t, path)))
			for _, identifier := range blocked {
				if strings.Contains(text, identifier) {
					t.Errorf("known lab fixture identifier %q remains in %s", identifier, path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
