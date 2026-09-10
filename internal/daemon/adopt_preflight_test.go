package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestConcurrentGatewayAdoptionsAdmitOnlyOneBootstrap(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "gateway-race"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	functions := model.DeviceFunctions{model.FunctionGateway}
	var bootstrapCalls atomic.Int32
	firstHasSlot := make(chan struct{})
	releaseFirst := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		release, err := d.beginAdoption(ctx, "192.0.2.1", functions,
			model.ManagementModeManaged)
		if err != nil {
			results <- err
			return
		}
		close(firstHasSlot)
		bootstrapCalls.Add(1)
		at := time.Now().Unix()
		err = d.Store.UpsertDevice(ctx, &store.Device{
			MAC: "aa:bb:cc:dd:ee:01", Host: "192.0.2.1", Name: "gateway-1",
			Role: "gateway", Functions: []string{"gateway"}, AdoptedAt: &at,
		})
		<-releaseFirst
		release()
		results <- err
	}()

	<-firstHasSlot
	secondStarted := make(chan struct{})
	go func() {
		defer wg.Done()
		close(secondStarted)
		release, err := d.beginAdoption(ctx, "192.0.2.2", functions,
			model.ManagementModeManaged)
		if err == nil {
			bootstrapCalls.Add(1)
			release()
		}
		results <- err
	}()
	<-secondStarted
	close(releaseFirst)
	wg.Wait()
	close(results)

	var gatewayConflict bool
	for err := range results {
		if err != nil && strings.Contains(err.Error(), "already the managed gateway") {
			gatewayConflict = true
		}
	}
	if !gatewayConflict {
		t.Fatal("the second gateway was not rejected by the pre-touch inventory check")
	}
	if got := bootstrapCalls.Load(); got != 1 {
		t.Fatalf("device bootstrap admitted %d times, want exactly once", got)
	}
}

func TestFirstAdoptionMayBeAPOnly(t *testing.T) {
	d, err := Open(context.Background(), testConfig(t, "external-gateway"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	release, err := d.beginAdoption(context.Background(), "192.0.2.2",
		model.DeviceFunctions{model.FunctionAP}, model.ManagementModeManaged)
	if err != nil {
		t.Fatalf("AP-only first adoption was refused: %v", err)
	}
	release()
}

func TestCorruptGatewayRowStillReservesTheGatewaySlot(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "corrupt-gateway-slot"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at := time.Now().Unix()
	dev := &store.Device{
		MAC: "aa:bb:cc:dd:ee:03", Host: "192.0.2.3", Name: "uncertain-gateway",
		Role: "gateway", Functions: []string{"gateway"}, AdoptedAt: &at,
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.SQL().ExecContext(ctx,
		`UPDATE devices SET functions_json='[]' WHERE id=?`, dev.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := d.Store.DeviceByID(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.FunctionError == "" {
		t.Fatal("fixture did not load as corrupt")
	}
	release, err := d.beginAdoption(ctx, "192.0.2.4",
		model.DeviceFunctions{model.FunctionGateway}, model.ManagementModeManaged)
	if err == nil {
		release()
		t.Fatal("corrupt existing gateway allowed a second gateway admission")
	}
}

func TestMonitorOnlyGatewaysDoNotConsumeManagedGatewaySlot(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "monitor-only-gateways"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at := time.Now().Unix()
	for i, host := range []string{"192.0.2.1", "198.51.100.1"} {
		dev := &store.Device{
			MAC: fmt.Sprintf("aa:bb:cc:dd:21:%02d", i), Host: host,
			Name: fmt.Sprintf("observed-router-%d", i), Role: "gateway",
			Functions: []string{"gateway"}, ManagementMode: "monitor_only", AdoptedAt: &at,
		}
		if err := d.Store.UpsertDevice(ctx, dev); err != nil {
			t.Fatal(err)
		}
	}
	release, err := d.beginAdoption(ctx, "203.0.113.1",
		model.DeviceFunctions{model.FunctionGateway}, model.ManagementModeManaged)
	if err != nil {
		t.Fatalf("monitor-only routers reserved the managed gateway slot: %v", err)
	}
	release()
}

func TestMonitorOnlyGatewayMayJoinExistingManagedGateway(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "monitor-after-managed"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at := time.Now().Unix()
	if err := d.Store.UpsertDevice(ctx, &store.Device{
		MAC: "aa:bb:cc:dd:21:10", Host: "192.0.2.1", Name: "managed-router",
		Role: "gateway", Functions: []string{"gateway"}, AdoptedAt: &at,
	}); err != nil {
		t.Fatal(err)
	}
	release, err := d.beginAdoption(ctx, "198.51.100.1",
		model.DeviceFunctions{model.FunctionGateway}, model.ManagementModeMonitorOnly)
	if err != nil {
		t.Fatalf("monitor-only router was refused: %v", err)
	}
	release()
}
