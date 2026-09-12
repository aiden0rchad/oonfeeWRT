package firmware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var testIdentity = Identity{BoardName: "vendor,router-one", Target: "test/small", RootFSType: "squashfs", Release: "OpenWrt 25.12.1 r100-test"}

func testManifest() map[string]any {
	return map[string]any{"target": "test/small", "version_number": "25.12.5", "profiles": map[string]any{
		"vendor_router-one": map[string]any{"supported_devices": []string{"vendor,router-one"}, "images": []any{
			map[string]any{"name": "openwrt-25.12.5-test-small-vendor_router-one-squashfs-sysupgrade.bin", "type": "sysupgrade", "filesystem": "squashfs", "sha256": strings.Repeat("ab", 32), "size": 123456},
		}},
	}}
}

func testChecker(t *testing.T, manifest any) (*Checker, *int) {
	t.Helper()
	count := 0
	c := NewChecker()
	c.now = func() time.Time { return time.Unix(1234, 0) }
	c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count++
		if r.URL.Scheme != "https" || r.URL.Host != "downloads.openwrt.org" {
			t.Fatalf("unexpected destination %s", r.URL)
		}
		var data any = manifest
		if r.URL.Path == "/.versions.json" {
			data = map[string]string{"stable_version": "25.12.5", "oldstable_version": "24.10.8"}
		} else if r.URL.Path != "/releases/25.12.5/targets/test/small/profiles.json" {
			t.Fatalf("unexpected catalogue path %s", r.URL.Path)
		}
		body, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})
	return c, &count
}

func TestExactBoardMaintenanceCandidate(t *testing.T) {
	c, calls := testChecker(t, testManifest())
	r := c.Check(context.Background(), testIdentity)
	if r.State != "available" || r.LatestVersion != "25.12.5" || r.Branch != "25.12" || r.CheckedAt != 1234000 || *calls != 2 {
		t.Fatalf("result %+v, calls %d", r, *calls)
	}
	if r.Image == nil || r.Image.SHA256 != strings.Repeat("ab", 32) || r.Image.Size != 123456 || !strings.HasPrefix(r.Image.URL, SourceURL+"/releases/25.12.5/targets/test/small/") {
		t.Fatalf("image metadata %+v", r.Image)
	}
	if len(r.Limitations) < 4 || !strings.Contains(r.Limitations[1], "not downloaded image bytes") {
		t.Fatal("missing metadata-only boundary")
	}
}

func TestCurrentAndAheadAreDifferent(t *testing.T) {
	for _, test := range []struct{ release, state string }{{"OpenWrt 25.12.5", "current"}, {"OpenWrt 25.12.6", "ahead"}} {
		t.Run(test.state, func(t *testing.T) {
			c, _ := testChecker(t, testManifest())
			identity := testIdentity
			identity.Release = test.release
			if r := c.Check(context.Background(), identity); r.State != test.state {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestUnprovenIdentityNeverSendsARequest(t *testing.T) {
	for _, identity := range []Identity{
		{}, {Release: "OpenWrt SNAPSHOT"}, {Release: "OtherWrt 25.12.1"},
		{Release: "OpenWrt 25.12.1-rc1"},
		{Release: testIdentity.Release, BoardName: "board", RootFSType: "squashfs", Target: "../private"},
		{Release: testIdentity.Release, BoardName: "board", RootFSType: "squashfs", Target: "x86/64"},
	} {
		c, calls := testChecker(t, testManifest())
		if r := c.Check(context.Background(), identity); r.State != "unsupported" || r.Image != nil || *calls != 0 {
			t.Fatalf("%+v: %+v calls %d", identity, r, *calls)
		}
	}
}

func TestOldBranchIsNotUpgradedAcrossVersions(t *testing.T) {
	c, calls := testChecker(t, testManifest())
	identity := testIdentity
	identity.Release = "OpenWrt 23.05.6"
	r := c.Check(context.Background(), identity)
	if r.State != "unsupported" || r.LatestVersion != "" || r.Image != nil || *calls != 1 {
		t.Fatalf("%+v calls %d", r, *calls)
	}
}

func TestRejectsAmbiguousOrUnsafeImageMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong target", func(m map[string]any) { m["target"] = "other/target" }},
		{"wrong release", func(m map[string]any) { m["version_number"] = "25.12.4" }},
		{"similar board is not proof", func(m map[string]any) { profile(m)["supported_devices"] = []string{"vendor,router-two"} }},
		{"two matching profiles", func(m map[string]any) { m["profiles"].(map[string]any)["another"] = profile(m) }},
		{"two matching images", func(m map[string]any) { profile(m)["images"] = []any{firstImage(m), firstImage(m)} }},
		{"factory image", func(m map[string]any) { firstImage(m)["type"] = "factory" }},
		{"different filesystem", func(m map[string]any) { firstImage(m)["filesystem"] = "ext4" }},
		{"path traversal", func(m map[string]any) { firstImage(m)["name"] = "../../attacker.bin" }},
		{"wrong checksum", func(m map[string]any) { firstImage(m)["sha256"] = "not-a-checksum" }},
		{"missing size", func(m map[string]any) { delete(firstImage(m), "size") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			test.mutate(manifest)
			c, _ := testChecker(t, manifest)
			r := c.Check(context.Background(), testIdentity)
			if r.Image != nil || (r.State != "unsupported" && r.State != "error") {
				t.Fatalf("unsafe candidate: %+v", r)
			}
		})
	}
}

func profile(m map[string]any) map[string]any {
	return m["profiles"].(map[string]any)["vendor_router-one"].(map[string]any)
}
func firstImage(m map[string]any) map[string]any {
	return profile(m)["images"].([]any)[0].(map[string]any)
}

func TestFailedCheckNeverReusesSuccessOrLeaksUpstreamErrors(t *testing.T) {
	c, _ := testChecker(t, testManifest())
	if r := c.Check(context.Background(), testIdentity); r.State != "available" {
		t.Fatal(r)
	}
	c.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("SECRET-SENTINEL in transport") })
	r := c.Check(context.Background(), testIdentity)
	encoded, _ := json.Marshal(r)
	if r.State != "error" || r.Image != nil || r.LatestVersion != "" || strings.Contains(string(encoded), "SECRET-SENTINEL") {
		t.Fatalf("stale or unsafe failure: %s", encoded)
	}
}

func TestMalformedAndOversizedCataloguesFailClosed(t *testing.T) {
	for _, body := range []string{"{}", "null", "broken-json", strings.Repeat(" ", maxManifestBytes+1)} {
		c := NewChecker()
		c.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if r := c.Check(context.Background(), testIdentity); r.State != "error" || r.Image != nil {
			t.Fatalf("unexpected result %+v", r)
		}
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	c := NewChecker()
	count := 0
	c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count++
		if r.URL.Host != "downloads.openwrt.org" {
			t.Fatalf("followed redirect: %s", r.URL)
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	if r := c.Check(context.Background(), testIdentity); r.State != "error" || count != 1 {
		t.Fatalf("%+v calls %d", r, count)
	}
}

func TestConcurrentChecksAreBounded(t *testing.T) {
	c := NewChecker()
	for range cap(c.slots) {
		c.slots <- struct{}{}
	}
	if r := c.Check(context.Background(), testIdentity); r.State != "error" || !strings.Contains(r.Message, "still running") {
		t.Fatalf("%+v", r)
	}
}
