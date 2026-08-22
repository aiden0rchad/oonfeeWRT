package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestCapabilityInstallLedgerSurvivesErrorsAndBlocksDeviceDeletion(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dev := &Device{MAC: "02:00:00:00:17:01", Host: "192.0.2.17", Name: "capability"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", []string{"lldpd"}, []string{"base-files"}, []CapabilityServiceState{{Name: "lldpd"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, dev.ID); err == nil {
		t.Fatal("device deletion discarded a live capability rollback record")
	}
	if err := db.CompleteCapabilityInstall(ctx, dev.ID, "lldp", []string{"lldpd", "libcap"}, []CapabilityServiceState{{Name: "lldpd"}}); err != nil {
		t.Fatal(err)
	}
	got, err := db.CapabilityInstall(ctx, dev.ID, "lldp")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.State != "installed" || len(got.BaselinePackages) != 1 || len(got.AddedPackages) != 2 || got.InstalledAt == nil {
		t.Fatalf("install=%+v", got)
	}
	if err := db.BeginCapabilityRemoval(ctx, dev.ID, "lldp"); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCapabilityRemoval(ctx, dev.ID, "lldp"); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if err := db.CompleteCapabilityRemoval(ctx, dev.ID, "lldp"); err != nil {
		t.Fatal(err)
	}
	if got, err := db.CapabilityInstall(ctx, dev.ID, "lldp"); err != nil || got != nil {
		t.Fatalf("removed install=%+v err=%v", got, err)
	}
}

func TestCapabilityServiceConfigBaselineRoundTrips(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dev := &Device{MAC: "02:00:00:00:17:03", Host: "192.0.2.19", Name: "configured"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", []string{"lldpd"}, []string{"base-files"}, []CapabilityServiceState{{Name: "lldpd"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteCapabilityInstall(ctx, dev.ID, "lldp", []string{"lldpd"}, []CapabilityServiceState{{Name: "lldpd"}}); err != nil {
		t.Fatal(err)
	}
	service := CapabilityServiceState{Name: "lldpd", ConfigBaseline: "package lldpd\n", ConfigApplied: "package lldpd\n\tlist interface 'lan3'\n", ConfiguredInterfaces: []string{"lan3"}}
	if err := db.UpdateCapabilityServices(ctx, dev.ID, "lldp", []CapabilityServiceState{service}); err != nil {
		t.Fatal(err)
	}
	got, err := db.CapabilityInstall(ctx, dev.ID, "lldp")
	if err != nil || len(got.Services) != 1 || got.Services[0].ConfigApplied != service.ConfigApplied ||
		strings.Join(got.Services[0].ConfiguredInterfaces, ",") != "lan3" {
		t.Fatalf("install=%+v err=%v", got, err)
	}
}

func TestCapabilityRemovalRecoversInterruptedInstall(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dev := &Device{MAC: "02:00:00:00:17:02", Host: "192.0.2.18", Name: "interrupted"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", []string{"lldpd"}, []string{"base-files"}, []CapabilityServiceState{{Name: "lldpd"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCapabilityRemoval(ctx, dev.ID, "lldp"); err != nil {
		t.Fatalf("recover interrupted install: %v", err)
	}
	if err := db.CompleteCapabilityRemoval(ctx, dev.ID, "lldp"); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityInstallRetryPreservesOriginalRollbackBaseline(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dev := &Device{MAC: "02:00:00:00:17:04", Host: "192.0.2.20", Name: "retry"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	originalServices := []CapabilityServiceState{{Name: "lldpd", WasEnabled: true}}
	if err := db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", []string{"lldpd"},
		[]string{"base-files"}, originalServices); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCapabilityAddedPackages(ctx, dev.ID, "lldp", []string{"libcap"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCapabilityInstallError(ctx, dev.ID, "lldp", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", []string{"lldpd"},
		[]string{"base-files", "libcap"}, []CapabilityServiceState{{Name: "lldpd"}}); err != nil {
		t.Fatal(err)
	}
	got, err := db.CapabilityInstall(ctx, dev.ID, "lldp")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "installing" || !slices.Equal(got.BaselinePackages, []string{"base-files"}) ||
		!slices.Equal(got.AddedPackages, []string{"libcap"}) || len(got.Services) != 1 || !got.Services[0].WasEnabled {
		t.Fatalf("retry replaced original rollback evidence: %+v", got)
	}
}

func TestCapabilityInstallRetryRejectsCompletedInstallError(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dev := &Device{MAC: "02:00:00:00:17:05", Host: "192.0.2.21", Name: "completed"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	services := []CapabilityServiceState{{Name: "lldpd"}}
	if err := db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", []string{"lldpd"}, []string{"base-files"}, services); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteCapabilityInstall(ctx, dev.ID, "lldp", []string{"lldpd"}, services); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCapabilityInstallError(ctx, dev.ID, "lldp", "configuration failed"); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", []string{"lldpd"}, nil, services); err == nil {
		t.Fatal("completed install error was accepted as a fresh package-install retry")
	}
}
