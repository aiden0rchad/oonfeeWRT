package deploy

import (
	"os"
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
		".env", ".env.*", "**/.env", "**/.env.*", "**/node_modules/", "ui/dist/", "dist/",
	} {
		if !lines[required] {
			t.Errorf(".dockerignore does not exclude %q", required)
		}
	}
	gitignore, err := os.ReadFile("../.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"keyring.json", "*.db", "*.db-*", ".env", ".env.*", "/dist/"} {
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
	for _, script := range []string{"../tools/release-build.sh", "../tools/reproducible-build-check.sh"} {
		info, err := os.Stat(script)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", script)
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

	compose, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, hardening := range []string{
		"read_only: true", "/tmp:rw,noexec,nosuid,nodev", "cap_drop:",
		"- ALL", "no-new-privileges:true", "create_host_path: false",
	} {
		if !strings.Contains(string(compose), hardening) {
			t.Errorf("compose lost runtime hardening %q", hardening)
		}
	}
}
