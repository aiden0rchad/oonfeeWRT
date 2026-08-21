package daemon

import (
	"context"
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
		release, err := d.beginAdoption(ctx, "192.0.2.1", functions)
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
		release, err := d.beginAdoption(ctx, "192.0.2.2", functions)
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
		model.DeviceFunctions{model.FunctionAP})
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
		model.DeviceFunctions{model.FunctionGateway})
	if err == nil {
		release()
		t.Fatal("corrupt existing gateway allowed a second gateway admission")
	}
}
