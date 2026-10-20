package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/deploy"
	"github.com/aiden0rchad/oonfeewrt/internal/adoption"
	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/testutil"
)

func TestRefreshACLRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, before, pin, mode, after, wantErr string
		wantWrites                              bool
		preserveFails                           bool
	}{
		{name: "missing managed ACL", before: "denied", wantWrites: true},
		{name: "missing monitor ACL", before: "denied", mode: "monitor_only", wantWrites: true},
		{name: "ubus permission status", before: "permission", wantWrites: true},
		{name: "ordinary refresh", wantWrites: true},
		{name: "ordinary legacy refresh", pin: "missing", wantWrites: true},
		{name: "recovery without SSH pin", before: "denied", pin: "missing", wantErr: "no saved SSH host key"},
		{name: "recovery with changed SSH key", before: "denied", pin: "changed", wantErr: "host key changed"},
		{name: "identity mismatch", before: "mismatch", wantErr: "endpoint identity"},
		{name: "identity source unavailable", before: "unsupported", wantErr: "verify device identity"},
		{name: "invalid identity response", before: "malformed", wantErr: "verify device identity"},
		{name: "identity transport failure", before: "transport", wantErr: "verify device identity"},
		{name: "controller login lost", before: "login", wantErr: "verify the existing controller login"},
		{name: "post-recovery identity mismatch", before: "denied", after: "mismatch", wantWrites: true, wantErr: "controller identity verification failed"},
		{name: "ACL still denied after write", before: "denied", after: "denied", wantWrites: true, wantErr: "controller identity verification failed"},
		{name: "capability verification failed", before: "denied", after: "probe", wantWrites: true, wantErr: "capability verification failed"},
		{name: "backup registration failed", before: "denied", preserveFails: true, wantWrites: true, wantErr: "preserve controller ACL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d := openDaemon(t)
			root := t.TempDir()
			for _, dir := range []string{"etc", "usr/share/rpcd/acl.d"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.preserveFails {
				if err := os.Mkdir(filepath.Join(root, "etc/sysupgrade.conf"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			var installed atomic.Bool
			var writes atomic.Int64
			mockURL, err := url.Parse("http://" + startMock(t))
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(mockURL)
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var call struct {
					ID     json.RawMessage   `json:"id"`
					Params []json.RawMessage `json:"params"`
				}
				if json.Unmarshal(body, &call) == nil && len(call.Params) == 4 {
					var object string
					_ = json.Unmarshal(call.Params[1], &object)
					if (tc.before == "login" && object == "session") ||
						(tc.after == "probe" && installed.Load() && object == "system") {
						fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":[6]}`, call.ID)
						return
					}
					if object == "network.device" {
						fault := tc.before
						if installed.Load() {
							fault = tc.after
						}
						w.Header().Set("Content-Type", "application/json")
						switch fault {
						case "denied":
							fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"Access denied"}}`, call.ID)
							return
						case "permission", "unsupported":
							status := 6
							if fault == "unsupported" {
								status = 4
							}
							fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":[%d]}`, call.ID, status)
							return
						case "mismatch":
							fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":[0,{"br-lan":{"macaddr":"02:00:00:00:00:99"}}]}`, call.ID)
							return
						case "malformed":
							fmt.Fprint(w, "invalid JSON")
							return
						case "transport":
							http.Error(w, "router unavailable", http.StatusServiceUnavailable)
							return
						}
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer httpServer.Close()
			server := testutil.StartSSH(t, strings.TrimPrefix(httpServer.URL, "http://"), func(command string, stdin io.Reader, stdout, stderr io.Writer) uint32 {
				preserve := strings.Contains(command, "conf=/etc/sysupgrade.conf")
				if strings.HasPrefix(command, "cat >") {
					writes.Add(1)
				}
				if !preserve {
					command = strings.ReplaceAll(command, "/usr/share/rpcd/acl.d/", root+"/usr/share/rpcd/acl.d/")
				}
				command = strings.ReplaceAll(command, "/etc/sysupgrade.conf", root+"/etc/sysupgrade.conf")
				cmd := exec.Command("sh", "-c", command)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
				if err := cmd.Run(); err != nil {
					return 1
				}
				if preserve {
					installed.Store(true)
				}
				return 0
			})
			dev := seedAP(t, d, "02:00:00:aa:bb:cc", "recoverable-ap", server.Addr, capability.Present)
			dev.HostKeyFP = server.Fingerprint
			if tc.pin == "missing" {
				dev.HostKeyFP = ""
			} else if tc.pin == "changed" {
				dev.HostKeyFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			}
			if tc.mode != "" {
				dev.ManagementMode = tc.mode
			}
			if err := d.Store.UpsertDevice(ctx, dev); err != nil {
				t.Fatal(err)
			}
			before, err := d.Store.DeviceByID(ctx, dev.ID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := d.RefreshACL(ctx, api.RefreshACLRequest{
				DeviceID: dev.ID, Username: "root", Password: "not-a-real-admin-password",
				AcknowledgeRouterChanges: true,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || result != nil {
					t.Fatalf("result=%+v, err=%v, want %q", result, err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "not-a-real-admin-password") {
					t.Fatalf("administrator credential exposed: %v", err)
				}
			} else if err != nil || result == nil || !result.ACLUpdated || !result.ControllerVerified {
				t.Fatalf("recovery result=%+v, err=%v", result, err)
			}
			if (writes.Load() > 0) != tc.wantWrites {
				t.Fatalf("ACL writes=%d, wantWrites=%t", writes.Load(), tc.wantWrites)
			}
			if !tc.wantWrites && server.Authentications.Load() != 0 {
				t.Fatalf("administrator credential sent before identity was proved: authentications=%d", server.Authentications.Load())
			}
			after, err := d.Store.DeviceByID(ctx, dev.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.CredEnc, after.CredEnc) || before.MAC != after.MAC || before.ManagementMode != after.ManagementMode || before.Name != after.Name {
				t.Fatal("ACL refresh changed the stored credential or inventory identity")
			}
			if tc.wantErr != "" && before.CapsJSON != after.CapsJSON {
				t.Fatal("failed verification replaced the stored capabilities")
			}
			if tc.wantWrites {
				wantACL := deploy.ACL
				if tc.mode == "monitor_only" {
					wantACL = deploy.MonitorACL
				}
				got, err := os.ReadFile(root + adoption.DefaultACLPath)
				if err != nil || !bytes.Equal(got, wantACL) {
					t.Fatalf("installed ACL has incorrect scope, err=%v", err)
				}
				if !tc.preserveFails {
					got, err = os.ReadFile(filepath.Join(root, "etc/sysupgrade.conf"))
					if err != nil || string(got) != adoption.DefaultACLPath+"\n" {
						t.Fatalf("recovered ACL is not preserved: %q, err=%v", got, err)
					}
				}
			}
			if tc.wantErr == "" && after.HostKeyFP != server.Fingerprint {
				t.Fatal("successful refresh did not retain the SSH identity pin")
			}
		})
	}
}
