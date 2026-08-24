package deploy

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseWorkflowContract(t *testing.T) {
	release := readWorkflow(t, "../.github/workflows/release.yml")
	for _, required := range []string{
		"gate:\n    name: Exact-SHA release gate",
		"ref: ${{ github.sha }}",
		"go test -count=1 ./...",
		"go vet ./...",
		"go test -race -count=1 ./...",
		"npm --prefix ui test",
		"npm --prefix ui run test:browser:install",
		"npm --prefix ui run test:browser",
		"npm --prefix ui audit --audit-level=high",
		"./tools/budget_check.sh",
		"govulncheck@v1.7.0",
		"./tools/secret-scan.sh",
		"./tools/full-secret-scan.sh",
		"./tools/reproducible-build-check.sh \"$GITHUB_REF_NAME\"",
		"./tools/release-container-smoke.sh \"$GITHUB_REF_NAME\"",
		"id-token: write",
		"sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6",
		"cosign sign --yes \"$IMAGE@$DIGEST\"",
		"--certificate-identity \"$identity\"",
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
		"echo \"$image:$GITHUB_REF_NAME\"",
		"echo \"$image:$stable\"",
		"echo \"$image:$minor\"",
		"echo \"$image:latest\"",
		"notes=\"RELEASE-NOTES-${GITHUB_REF_NAME}.md\"",
		"./tools/release-build.sh \"$GITHUB_REF_NAME\" \"$release_dir\"",
		"path: ${{ runner.temp }}/release/*",
		"--notes-file dist/RELEASE-NOTES.md",
		"needs: [gate, container]",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("release workflow lost %q", required)
		}
	}
	if strings.Count(release, "needs: gate") != 1 {
		t.Error("container publication must depend directly on the release gate")
	}
	if strings.Count(release, "ref: ${{ github.sha }}") != 2 {
		t.Error("every source-consuming release job must check out the event's exact SHA")
	}
	if strings.Contains(release, "\n  archives:\n") {
		t.Error("release archives must be built once in the gate, not rebuilt for publication")
	}
	if strings.Contains(release, `notes="RELEASE-NOTES-${GITHUB_REF_NAME#v}.md"`) {
		t.Error("release notes filename must retain the tag's leading v")
	}

	ci := readWorkflow(t, "../.github/workflows/ci.yml")
	for _, required := range []string{
		"./tools/reproducible-build-check.sh v0.0.0-ci",
		"govulncheck@v1.7.0",
		"npm --prefix ui audit --audit-level=high",
		"npm --prefix ui run test:browser:install",
		"npm --prefix ui run test:browser",
		"./tools/secret-scan.sh",
		"./tools/full-secret-scan.sh",
		"./tools/release-container-smoke.sh v0.0.0-ci",
	} {
		if !strings.Contains(ci, required) {
			t.Errorf("CI workflow lost %q", required)
		}
	}
	if strings.Contains(ci, "\n  vulnerabilities:\n") {
		t.Error("vulnerability scans must remain inside the five protected CI job contexts")
	}

	pinned := regexp.MustCompile(`^uses: (actions|docker|sigstore)/[^@[:space:]]+@[0-9a-f]{40}(?:[[:space:]]+#.*)?$`)
	for name, workflow := range map[string]string{"ci": ci, "release": release} {
		scanner := bufio.NewScanner(strings.NewReader(workflow))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "uses:") && !pinned.MatchString(line) {
				t.Errorf("%s workflow action is not an official immutable pin: %q", name, line)
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	}
}

func readWorkflow(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
