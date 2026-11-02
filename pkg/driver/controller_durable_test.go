// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

const testGiB = int64(1 << 30)

var testProviderSequence atomic.Uint64

type durableTestProvider struct {
	mu               sync.Mutex
	minimumBytes     int64
	failFirstCreate  bool
	terminalFailures int
	committed        bool
	operationID      string
	operationIDs     []string
	createCalls      int
	effectiveCreates int
	deleteCalls      int
}

type durableTerminalError struct{}

func (durableTerminalError) Error() string               { return "explicit cloud API rejection" }
func (durableTerminalError) TerminalCreateFailure() bool { return true }

type genericTestProvider struct {
	mu          sync.Mutex
	failures    int
	createCalls int
}

func (fake *genericTestProvider) CreateVolume(_ context.Context, volumeID string, size int64) (*provider.VolumeInfo, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.createCalls++
	if fake.failures > 0 {
		fake.failures--
		return nil, errors.New("conclusive provider failure")
	}
	return &provider.VolumeInfo{
		VolumeID: volumeID, Path: "disk-" + volumeID, SizeBytes: size,
		Provider: "generic-test", Metadata: map[string]string{"cloud-volume-path": "disk-" + volumeID},
	}, nil
}

func (*genericTestProvider) DeleteVolume(context.Context, string) error { return nil }
func (*genericTestProvider) GetVolumeInfo(context.Context, string) (*provider.VolumeInfo, error) {
	return nil, errors.New("not implemented")
}
func (*genericTestProvider) VolumeExists(context.Context, string) (bool, error) { return false, nil }

func registerGenericTestProvider(t *testing.T, fake *genericTestProvider) string {
	t.Helper()
	name := fmt.Sprintf("generic-test-%d", testProviderSequence.Add(1))
	provider.RegisterProvider(name, func(map[string]string) (provider.BlockVolumeProvider, error) {
		return fake, nil
	})
	return name
}

func registerDurableTestProvider(t *testing.T, fake *durableTestProvider) string {
	t.Helper()
	name := fmt.Sprintf("durable-test-%d", testProviderSequence.Add(1))
	provider.RegisterProvider(name, func(map[string]string) (provider.BlockVolumeProvider, error) {
		return fake, nil
	})
	return name
}

func (fake *durableTestProvider) NormalizeCapacity(required int64) (int64, error) {
	if required < fake.minimumBytes {
		return fake.minimumBytes, nil
	}
	return required, nil
}

func (fake *durableTestProvider) CreateVolume(context.Context, string, int64) (*provider.VolumeInfo, error) {
	return nil, errors.New("persisted operation ID required")
}

func (fake *durableTestProvider) CreateVolumeWithOperation(_ context.Context, volumeID string, size int64, operationID string) (*provider.VolumeInfo, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.createCalls++
	fake.operationIDs = append(fake.operationIDs, operationID)
	if fake.terminalFailures > 0 {
		fake.terminalFailures--
		return nil, durableTerminalError{}
	}
	if !fake.committed {
		fake.committed = true
		fake.operationID = operationID
		fake.effectiveCreates++
	} else if fake.operationID != operationID {
		fake.effectiveCreates++
	}
	if fake.failFirstCreate {
		fake.failFirstCreate = false
		return nil, errors.New("client timeout after service commit")
	}
	return &provider.VolumeInfo{
		VolumeID: volumeID, Path: "disk-" + volumeID, SizeBytes: size,
		Provider: "durable-test", Metadata: map[string]string{"cloud-volume-path": "disk-" + volumeID},
	}, nil
}

func (fake *durableTestProvider) DeleteVolume(context.Context, string) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.deleteCalls++
	fake.committed = false
	return nil
}

func (fake *durableTestProvider) GetVolumeInfo(context.Context, string) (*provider.VolumeInfo, error) {
	return nil, errors.New("not implemented")
}

func (fake *durableTestProvider) VolumeExists(context.Context, string) (bool, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.committed, nil
}

func filesystemCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func durableCreateRequest(name, providerName string, required, limit int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name: name, Parameters: map[string]string{"cloudProvider": providerName, "zoneId": "zone-a"},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: required, LimitBytes: limit},
		VolumeCapabilities: []*csi.VolumeCapability{filesystemCapability()},
	}
}

func newDurableController(t *testing.T, dir string) *controllerServer {
	t.Helper()
	store, err := newVolumeStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &controllerServer{store: store}
}

func TestCreateVolumeSerializesConcurrentIdenticalRequests(t *testing.T) {
	fake := &durableTestProvider{minimumBytes: 20 * testGiB}
	providerName := registerDurableTestProvider(t, fake)
	controller := newDurableController(t, t.TempDir())
	request := durableCreateRequest("workspace", providerName, testGiB, 25*testGiB)

	const workers = 20
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := controller.CreateVolume(context.Background(), request)
			if err == nil && response.GetVolume().GetCapacityBytes() != 20*testGiB {
				err = fmt.Errorf("capacity=%d", response.GetVolume().GetCapacityBytes())
			}
			errorsByWorker <- err
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 1 || fake.effectiveCreates != 1 {
		t.Fatalf("create calls=%d effective=%d, want 1/1", fake.createCalls, fake.effectiveCreates)
	}
}

func TestCreateVolumeResumesPendingOperationAfterRestart(t *testing.T) {
	fake := &durableTestProvider{minimumBytes: 20 * testGiB, failFirstCreate: true}
	providerName := registerDurableTestProvider(t, fake)
	dir := t.TempDir()
	request := durableCreateRequest("workspace", providerName, testGiB, 25*testGiB)

	first := newDurableController(t, dir)
	if _, err := first.CreateVolume(context.Background(), request); status.Code(err) != codes.Internal {
		t.Fatalf("first create status=%v, want Internal", status.Code(err))
	}
	pending, err := first.store.Load("workspace")
	if err != nil || pending.State != volumeRecordStatePending {
		t.Fatalf("pending record=(%+v, %v)", pending, err)
	}

	second := newDurableController(t, dir)
	response, err := second.CreateVolume(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetVolume().GetCapacityBytes() != 20*testGiB {
		t.Fatalf("capacity=%d", response.GetVolume().GetCapacityBytes())
	}
	fake.mu.Lock()
	if fake.createCalls != 2 || fake.effectiveCreates != 1 || fake.operationID != pending.OperationID {
		t.Fatalf("calls=%d effective=%d operation=(%s,%s)", fake.createCalls, fake.effectiveCreates, fake.operationID, pending.OperationID)
	}
	fake.mu.Unlock()

	if _, err := second.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "workspace"}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deleteCalls != 1 || fake.committed {
		t.Fatalf("delete calls=%d committed=%t", fake.deleteCalls, fake.committed)
	}
}

func TestCreateVolumeRejectsIncompatibleSameName(t *testing.T) {
	fake := &durableTestProvider{minimumBytes: 20 * testGiB}
	providerName := registerDurableTestProvider(t, fake)
	controller := newDurableController(t, t.TempDir())
	request := durableCreateRequest("workspace", providerName, testGiB, 25*testGiB)
	if _, err := controller.CreateVolume(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	changedZone := durableCreateRequest("workspace", providerName, testGiB, 25*testGiB)
	changedZone.Parameters["zoneId"] = "zone-b"
	if _, err := controller.CreateVolume(context.Background(), changedZone); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed zone status=%v, want AlreadyExists", status.Code(err))
	}
	changedSize := durableCreateRequest("workspace", providerName, 21*testGiB, 25*testGiB)
	if _, err := controller.CreateVolume(context.Background(), changedSize); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed size status=%v, want AlreadyExists", status.Code(err))
	}
}

func TestCreateVolumeRejectsProviderMinimumAboveLimit(t *testing.T) {
	fake := &durableTestProvider{minimumBytes: 20 * testGiB}
	providerName := registerDurableTestProvider(t, fake)
	controller := newDurableController(t, t.TempDir())
	request := durableCreateRequest("workspace", providerName, testGiB, 10*testGiB)
	if _, err := controller.CreateVolume(context.Background(), request); status.Code(err) != codes.OutOfRange {
		t.Fatalf("status=%v, want OutOfRange", status.Code(err))
	}
	if _, err := controller.store.Load("workspace"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capacity rejection persisted a pending operation")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 0 {
		t.Fatalf("capacity rejection made %d cloud mutations", fake.createCalls)
	}
}

func TestCreateVolumeRetiresExplicitFailureOperation(t *testing.T) {
	fake := &durableTestProvider{minimumBytes: testGiB, terminalFailures: 1}
	providerName := registerDurableTestProvider(t, fake)
	controller := newDurableController(t, t.TempDir())
	request := durableCreateRequest("workspace", providerName, testGiB, 0)
	if _, err := controller.CreateVolume(context.Background(), request); status.Code(err) != codes.Internal {
		t.Fatalf("first status=%v, want Internal", status.Code(err))
	}
	if _, err := controller.store.Load("workspace"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal failure left pending state: %v", err)
	}
	if _, err := controller.CreateVolume(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.operationIDs) != 2 || fake.operationIDs[0] == fake.operationIDs[1] || fake.effectiveCreates != 1 {
		t.Fatalf("operations=%v effective=%d", fake.operationIDs, fake.effectiveCreates)
	}
}

func TestCreateVolumeDoesNotPersistGenericProviderFailure(t *testing.T) {
	fake := &genericTestProvider{failures: 1}
	providerName := registerGenericTestProvider(t, fake)
	controller := newDurableController(t, t.TempDir())
	request := durableCreateRequest("workspace", providerName, testGiB, 0)

	if _, err := controller.CreateVolume(context.Background(), request); status.Code(err) != codes.Internal {
		t.Fatalf("first status=%v, want Internal", status.Code(err))
	}
	if _, err := controller.store.Load("workspace"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generic provider failure left controller state: %v", err)
	}
	request.Parameters["zoneId"] = "zone-b"
	if _, err := controller.CreateVolume(context.Background(), request); err != nil {
		t.Fatalf("corrected retry failed: %v", err)
	}
	record, err := controller.store.Load("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != volumeRecordStateReady || record.OperationID != "" {
		t.Fatalf("generic ready record=%+v", record)
	}
}

func TestCreateVolumeValidatesCDHParameterPair(t *testing.T) {
	fake := &genericTestProvider{}
	providerName := registerGenericTestProvider(t, fake)
	controller := newDurableController(t, t.TempDir())
	for _, params := range []map[string]string{
		{"encrypt-type": "luks2"},
		{"kbs-key-id": "default/luks-key/workspace"},
	} {
		request := durableCreateRequest("workspace", providerName, testGiB, 0)
		for key, value := range params {
			request.Parameters[key] = value
		}
		if _, err := controller.CreateVolume(context.Background(), request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("params=%v status=%v, want InvalidArgument", params, status.Code(err))
		}
	}
}

func TestCreateFingerprintIncludesTopologyAndPlannedCapacity(t *testing.T) {
	request := durableCreateRequest("workspace", "fingerprint-only", testGiB, 0)
	request.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{"zone": "zone-a"}}},
	}
	first, err := createRequestFingerprint(request, 20*testGiB)
	if err != nil {
		t.Fatal(err)
	}
	request.AccessibilityRequirements.Requisite[0].Segments["zone"] = "zone-b"
	second, err := createRequestFingerprint(request, 20*testGiB)
	if err != nil {
		t.Fatal(err)
	}
	third, err := createRequestFingerprint(request, 21*testGiB)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second == third {
		t.Fatalf("fingerprints do not bind topology/capacity: %s %s %s", first, second, third)
	}
}
