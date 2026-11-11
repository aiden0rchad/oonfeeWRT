package adoption

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/testutil"
)

func aclFilesystem(t *testing.T) (*SSHBootstrap, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"etc", "usr/share/rpcd/acl.d", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Only the login's absence matters to these footprint-removal tests.
	if err := os.WriteFile(filepath.Join(root, "bin/uci"), []byte("#!/bin/sh\n[ \"$2\" != get ]\n"), 0755); err != nil {
		t.Fatal(err)
	}
	server := testutil.StartSSH(t, "", func(command string, stdin io.Reader, stdout, stderr io.Writer) uint32 {
		if !strings.Contains(command, "conf=/etc/sysupgrade.conf") {
			command = strings.ReplaceAll(command, "/usr/share/rpcd/acl.d/", root+"/usr/share/rpcd/acl.d/")
		}
		command = strings.ReplaceAll(command, "/etc/sysupgrade.conf", root+"/etc/sysupgrade.conf")
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = append(os.Environ(), "PATH="+root+"/bin:"+os.Getenv("PATH"))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			return 1
		}
		return 0
	})
	boot, err := DialSSH(context.Background(), SSHOptions{Host: server.Addr, HostKeyFP: server.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boot.Close() })
	return boot, root
}

func TestACLBackupEntryLifecycle(t *testing.T) {
	const other = "# custom backup files\n# " + DefaultACLPath + "\n/etc/custom.conf\n/usr/share/rpcd/acl.d/oonfeewrtXjson\n"
	for _, tc := range []struct {
		name, before, remaining string
		missing                 bool
	}{
		{name: "missing file", missing: true},
		{name: "empty file"},
		{name: "operator entries", before: other, remaining: other},
		{name: "no trailing newline", before: "/etc/custom.conf", remaining: "/etc/custom.conf\n"},
		{name: "existing entry", before: other + DefaultACLPath + "\n", remaining: other},
		{name: "duplicate entries", before: DefaultACLPath + "\n" + other + DefaultACLPath + "\n", remaining: other},
	} {
		for _, footprint := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/ACL rollback", true: "/unadopt"}[footprint], func(t *testing.T) {
				boot, root := aclFilesystem(t)
				conf := filepath.Join(root, "etc/sysupgrade.conf")
				if !tc.missing {
					if err := os.WriteFile(conf, []byte(tc.before), 0640); err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					if err := boot.InstallACL(context.Background(), DefaultACLPath, []byte(`{"acl":"test"}`)); err != nil {
						t.Fatal(err)
					}
					got, err := os.ReadFile(conf)
					if err != nil || string(got) != tc.remaining+DefaultACLPath+"\n" {
						t.Fatalf("backup list = %q, err=%v", got, err)
					}
				}
				info, err := os.Stat(conf)
				if err != nil {
					t.Fatal(err)
				}
				wantMode := os.FileMode(0640)
				if tc.missing {
					wantMode = 0644
				}
				if info.Mode().Perm() != wantMode {
					t.Fatalf("backup list permissions = %o, want %o", info.Mode().Perm(), wantMode)
				}
				for range 2 {
					var err error
					if footprint {
						err = boot.RemoveFootprint(context.Background(), DefaultACLPath, DefaultUser)
					} else {
						err = boot.RemoveACL(context.Background(), DefaultACLPath)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				got, err := os.ReadFile(conf)
				if err != nil || string(got) != tc.remaining {
					t.Fatalf("cleanup changed unrelated entries: %q, err=%v", got, err)
				}
				if _, err := os.Stat(root + DefaultACLPath); !os.IsNotExist(err) {
					t.Fatalf("ACL still exists after cleanup: %v", err)
				}
			})
		}
	}
}

func TestACLRemovalWithoutBackupListDoesNotCreateOne(t *testing.T) {
	boot, root := aclFilesystem(t)
	if err := boot.RemoveACL(context.Background(), DefaultACLPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/sysupgrade.conf")); !os.IsNotExist(err) {
		t.Fatalf("cleanup created a backup list: %v", err)
	}
}

func TestACLBackupWriteFailurePreservesOperatorListAndReportsFailure(t *testing.T) {
	boot, root := aclFilesystem(t)
	conf := filepath.Join(root, "etc/sysupgrade.conf")
	const original = "# keep this exactly\n/etc/custom.conf"
	if err := os.WriteFile(conf, []byte(original), 0640); err != nil {
		t.Fatal(err)
	}
	// Fail only the backup-list rename, after the ACL itself was installed.
	script := "#!/bin/sh\nfor dest do :; done\ncase \"$dest\" in */etc/sysupgrade.conf) exit 1;; esac\nexec /bin/mv \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "bin/mv"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := boot.InstallACL(context.Background(), DefaultACLPath, []byte("acl")); err == nil || !strings.Contains(err.Error(), "preserve controller ACL") {
		t.Fatalf("backup registration failure was hidden: %v", err)
	}
	got, err := os.ReadFile(conf)
	if err != nil || string(got) != original {
		t.Fatalf("failed write damaged the original list: %q, err=%v", got, err)
	}
	if err := boot.RemoveACL(context.Background(), DefaultACLPath); err == nil || !strings.Contains(err.Error(), "backup entry") {
		t.Fatalf("backup cleanup failure was hidden: %v", err)
	}
	leftovers, err := filepath.Glob(conf + ".oonfee.*")
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary backup lists remain: %v, err=%v", leftovers, err)
	}
}

func TestACLBackupListSymlinkIsNotReplaced(t *testing.T) {
	boot, root := aclFilesystem(t)
	target := filepath.Join(root, "etc/operator-list")
	if err := os.WriteFile(target, []byte("/etc/private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "etc/sysupgrade.conf")); err != nil {
		t.Fatal(err)
	}
	if err := boot.InstallACL(context.Background(), DefaultACLPath, []byte("acl")); err == nil {
		t.Fatal("backup registration replaced an operator's symlink")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "/etc/private\n" {
		t.Fatalf("operator list changed: %q, err=%v", got, err)
	}
}
