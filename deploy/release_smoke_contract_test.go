package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseContainerSmokeContract(t *testing.T) {
	path := "../tools/release-container-smoke.sh"
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("release container smoke script is not executable")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"daemon_arch=$(docker info --format '{{.Architecture}}'",
		"amd64|x86_64) smoke_arch=amd64",
		"arm64|aarch64) smoke_arch=arm64",
		"unsupported Docker architecture",
		`CGO_ENABLED=0 GOOS=linux GOARCH="$smoke_arch"`,
		`--platform "linux/$smoke_arch"`,
		"./tools/releasecontainersmoke",
		"--network none",
		"OONFEE_LISTEN=127.0.0.1:$port",
		"--read-only",
		"--cap-drop ALL",
		"no-new-privileges:true",
		"target=/run/secrets/release-smoke.json,readonly",
		"target=/releasecontainersmoke,readonly",
		"docker exec \"$container\" /releasecontainersmoke /run/secrets/release-smoke.json",
		"chmod 0600 \"$runtime_file\" \"$config_file\"",
		"clear_file \"$config_file\"",
		"image_size",
	} {
		if !strings.Contains(string(body), required) {
			t.Errorf("release container smoke lost %q", required)
		}
	}
	for _, forbidden := range []string{"curl ", "jq ", "--network host"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("release container smoke retained host-side dependency %q", forbidden)
		}
	}

	helper, err := os.ReadFile("../tools/releasecontainersmoke/main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"config must be a private regular file",
		`u.Hostname() != "127.0.0.1"`,
		`"/setup"`, `"/devices"`, `"/backups"`, `"/download"`, `"/accounts"`,
		`"/restores"`, `"/restores/uploads"`, `"/restores/previews"`, `"/confirm"`,
		`"/restores/suppression"`,
		"acknowledge_sensitive_content", "acknowledge_restart",
		"acknowledge_session_revocation", "acknowledge_router_writes_suppressed",
		"acknowledge_no_automatic_router_apply", "destination_runtime_passphrase",
		"backup download checksum mismatch", "account rollback invariant failed",
		"restore preview did not bind the expected zero-device schema-20 plan",
		`X-OonfeeWRT-Instance`,
		"restored router-write suppression does not match the accepted intent",
	} {
		if !strings.Contains(string(helper), required) {
			t.Errorf("in-container release helper lost %q", required)
		}
	}
}
