// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var csLogger = log.New(log.Writer(), "[caa-csi/controller] ", log.LstdFlags|log.Lmsgprefix)

type controllerServer struct {
	csi.UnimplementedControllerServer
	store *volumeStore
	locks volumeNameLocks
}

func newControllerServer(store *volumeStore) *controllerServer {
	if params := store.BootstrapParams(); params != nil {
		if err := store.RecoverFromCloud(context.Background(), params); err != nil {
			csLogger.Printf("cloud recovery failed (non-fatal): %v", err)
		}
	} else {
		csLogger.Printf("No bootstrap params available, skipping cloud recovery (set CSI_CLOUD_PROVIDER / CSI_BOOTSTRAP_PARAMS_FILE for empty-store recovery)")
	}

	return &controllerServer{store: store}
}

// loadVolumeRecord returns the local record, attempting cloud recovery first
// when the record is missing.
func (cs *controllerServer) loadVolumeRecord(ctx context.Context, volumeID string) (*volumeRecord, error) {
	rec, err := cs.store.Load(volumeID)
	if err == nil {
		return rec, nil
	}

	if params := cs.store.BootstrapParams(); params != nil {
		if recErr := cs.store.RecoverFromCloud(ctx, params); recErr != nil {
			csLogger.Printf("cloud recovery before loading %s failed: %v", volumeID, recErr)
		}
		if rec, err2 := cs.store.Load(volumeID); err2 == nil {
			return rec, nil
		}
	}
	return nil, err
}

// resolveProviderParams picks cloud params for operations that may lack a
// local volume record (delete, snapshot delete).
func (cs *controllerServer) resolveProviderParams(secrets map[string]string) map[string]string {
	if len(secrets) > 0 && secrets["cloudProvider"] != "" {
		return secrets
	}
	return cs.store.BootstrapParams()
}

// volumeLookupStatus maps store/lookup errors to gRPC status codes:
// missing record → NotFound; corrupt/unreadable/other → Internal.
func volumeLookupStatus(kind, volumeID string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return status.Errorf(codes.NotFound, "%s %s not found", kind, volumeID)
	}
	return status.Errorf(codes.Internal, "loading %s %s: %v", kind, volumeID, err)
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume name missing")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing")
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported volume capability: %v", err)
	}
	if len(req.GetMutableParameters()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "mutable parameters are not supported")
	}

	params := sanitizePersistableParams(req.GetParameters())
	applyTopologyParams(params, req.GetAccessibilityRequirements())
	encryptType, keyID := params["encrypt-type"], params["kbs-key-id"]
	if (encryptType == "") != (keyID == "") {
		return nil, status.Error(codes.InvalidArgument,
			"encrypt-type and kbs-key-id must be set together")
	}
	required, limit, err := capacityRange(req)
	if err != nil {
		return nil, status.Errorf(codes.OutOfRange, "invalid capacity range: %v", err)
	}

	unlock, err := cs.locks.lock(ctx, req.GetName())
	if err != nil {
		return nil, status.FromContextError(err).Err()
	}
	defer unlock()

	record, loadErr := cs.store.Load(req.GetName())
	if loadErr == nil && record.State != volumeRecordStatePending {
		if record.SchemaVersion == 0 {
			err = legacyRecordCompatible(record, params, required, limit)
		} else {
			fingerprint, fingerprintErr := createRequestFingerprint(req, record.PlannedCapacityBytes)
			if fingerprintErr != nil {
				return nil, status.Errorf(codes.InvalidArgument, "invalid create request: %v", fingerprintErr)
			}
			err = requestFitsRecord(record, fingerprint, required, limit)
		}
		if err != nil {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %s already exists incompatibly: %v", req.GetName(), err)
		}
		csLogger.Printf("CreateVolume: %s already exists, returning existing", req.GetName())
		return createVolumeResponse(record, req.GetVolumeContentSource()), nil
	}
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "failed to load controller state for volume %s: %v", req.GetName(), loadErr)
	}

	var p provider.BlockVolumeProvider
	pendingPersisted := loadErr == nil
	if loadErr == nil {
		fingerprint, fingerprintErr := createRequestFingerprint(req, record.PlannedCapacityBytes)
		if fingerprintErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid create request: %v", fingerprintErr)
		}
		if err := requestFitsRecord(record, fingerprint, required, limit); err != nil {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %s has an incompatible pending create: %v", req.GetName(), err)
		}
		params = copyStringMap(record.Params)
		p, err = provider.NewBlockVolumeProvider(params)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to resume provider operation: %v", err)
		}
		if err := validateVolumeContentSource(ctx, p, req.GetVolumeContentSource()); err != nil {
			return nil, err
		}
		if req.GetVolumeContentSource() != nil {
			return nil, status.Error(codes.FailedPrecondition,
				"persisted create operation unexpectedly contains a content source")
		}
		if _, ok := p.(provider.OperationVolumeCreator); !ok {
			return nil, status.Error(codes.FailedPrecondition,
				"persisted create operation is not supported by its provider")
		}
	} else {
		p, err = provider.NewBlockVolumeProvider(params)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to create provider: %v", err)
		}
		plannedCapacity := required
		if planner, ok := p.(provider.CapacityPlanner); ok {
			plannedCapacity, err = planner.NormalizeCapacity(required)
			if err != nil {
				return nil, status.Errorf(codes.OutOfRange, "provider cannot satisfy capacity range: %v", err)
			}
		}
		if plannedCapacity < required || (limit > 0 && plannedCapacity > limit) {
			return nil, status.Errorf(codes.OutOfRange,
				"provider capacity %d is outside requested range [%d,%d]", plannedCapacity, required, limit)
		}
		if err := validateVolumeContentSource(ctx, p, req.GetVolumeContentSource()); err != nil {
			return nil, err
		}
		fingerprint, err := createRequestFingerprint(req, plannedCapacity)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid create request: %v", err)
		}
		record = &volumeRecord{
			SchemaVersion:        volumeRecordSchemaVersion,
			State:                volumeRecordStatePending,
			VolumeID:             req.GetName(),
			Provider:             params["cloudProvider"],
			PlannedCapacityBytes: plannedCapacity,
			Params:               params,
			RequestFingerprint:   fingerprint,
		}
		// Only providers with an explicit durable operation identity may leave
		// pending state. Other providers retain their existing idempotency
		// contract and are recorded only after CreateVolume succeeds.
		if _, ok := p.(provider.OperationVolumeCreator); ok && req.GetVolumeContentSource() == nil {
			record.OperationID, err = newVolumeOperationID()
			if err != nil {
				return nil, status.Errorf(codes.Internal, "%v", err)
			}
			if err := cs.store.Save(record); err != nil {
				return nil, status.Errorf(codes.Internal,
					"failed to persist create operation before cloud mutation: %v", err)
			}
			pendingPersisted = true
		}
	}

	var volInfo *provider.VolumeInfo
	if src := req.GetVolumeContentSource(); src != nil {
		switch {
		case src.GetSnapshot() != nil:
			snapID := src.GetSnapshot().GetSnapshotId()
			snapshotter, ok := p.(provider.VolumeSnapshotter)
			if !ok {
				return nil, status.Errorf(codes.NotFound, "snapshot %s not found", snapID)
			}
			snap, findErr := snapshotter.FindSnapshot(ctx, snapID)
			if findErr != nil {
				return nil, status.Errorf(codes.Internal, "failed to verify snapshot %s: %v", snapID, findErr)
			}
			if snap == nil {
				return nil, status.Errorf(codes.NotFound, "snapshot %s not found", snapID)
			}
			cloner, ok := p.(provider.VolumeCloner)
			if !ok {
				return nil, status.Errorf(codes.Unimplemented, "provider does not support creating volumes from snapshot")
			}
			csLogger.Printf("CreateVolume: %s from snapshot %s", req.GetName(), snapID)
			volInfo, err = cloner.CreateVolumeFromSnapshot(ctx, req.GetName(), snapID, record.PlannedCapacityBytes)
		case src.GetVolume() != nil:
			srcVolID := src.GetVolume().GetVolumeId()
			exists, existsErr := p.VolumeExists(ctx, srcVolID)
			if existsErr != nil {
				return nil, status.Errorf(codes.Internal, "failed to verify source volume %s: %v", srcVolID, existsErr)
			}
			if !exists {
				return nil, status.Errorf(codes.NotFound, "source volume %s not found", srcVolID)
			}
			cloner, ok := p.(provider.VolumeCloner)
			if !ok {
				return nil, status.Errorf(codes.Unimplemented, "provider does not support volume cloning")
			}
			csLogger.Printf("CreateVolume: %s cloned from volume %s", req.GetName(), srcVolID)
			volInfo, err = cloner.CreateVolumeFromVolume(ctx, req.GetName(), srcVolID, record.PlannedCapacityBytes)
		default:
			return nil, status.Error(codes.InvalidArgument, "unsupported VolumeContentSource type")
		}
	} else {
		if creator, ok := p.(provider.OperationVolumeCreator); ok {
			volInfo, err = creator.CreateVolumeWithOperation(ctx, req.GetName(), record.PlannedCapacityBytes, record.OperationID)
		} else {
			volInfo, err = p.CreateVolume(ctx, req.GetName(), record.PlannedCapacityBytes)
		}
	}
	if err != nil {
		var terminal provider.TerminalCreateError
		if pendingPersisted && errors.As(err, &terminal) && terminal.TerminalCreateFailure() {
			if deleteErr := cs.store.Delete(req.GetName()); deleteErr != nil {
				return nil, status.Errorf(codes.Internal,
					"provider rejected create and pending state cleanup failed: %v (provider error: %v)", deleteErr, err)
			}
		}
		return nil, status.Errorf(codes.Internal, "provider.CreateVolume failed: %v", err)
	}

	if err := validateCreatedVolume(req.GetName(), volInfo); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if volInfo.SizeBytes < required || (limit > 0 && volInfo.SizeBytes > limit) {
		return nil, status.Errorf(codes.OutOfRange,
			"provider allocated %d bytes outside requested range [%d,%d]", volInfo.SizeBytes, required, limit)
	}
	record.State = volumeRecordStateReady
	record.Provider = volInfo.Provider
	record.Path = volInfo.Path
	record.CapacityBytes = volInfo.SizeBytes
	record.Metadata = volInfo.Metadata
	if err := cs.store.Save(record); err != nil {
		return nil, status.Errorf(codes.Internal,
			"cloud volume %s exists, but ready state persistence failed; retry will resume operation: %v", req.GetName(), err)
	}
	csLogger.Printf("CreateVolume: %s created (provider=%s)", req.GetName(), volInfo.Provider)
	return createVolumeResponse(record, req.GetVolumeContentSource()), nil
}

func validateVolumeContentSource(ctx context.Context, p provider.BlockVolumeProvider, source *csi.VolumeContentSource) error {
	if source == nil {
		return nil
	}
	_, supportsClone := p.(provider.VolumeCloner)
	switch {
	case source.GetSnapshot() != nil:
		snapshotID := source.GetSnapshot().GetSnapshotId()
		snapshotter, ok := p.(provider.VolumeSnapshotter)
		if !ok {
			return status.Errorf(codes.NotFound, "snapshot %s not found", snapshotID)
		}
		snapshot, err := snapshotter.FindSnapshot(ctx, snapshotID)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to verify snapshot %s: %v", snapshotID, err)
		}
		if snapshot == nil {
			return status.Errorf(codes.NotFound, "snapshot %s not found", snapshotID)
		}
		if !supportsClone {
			return status.Error(codes.Unimplemented, "provider does not support creating volumes from snapshot")
		}
	case source.GetVolume() != nil:
		sourceVolumeID := source.GetVolume().GetVolumeId()
		exists, err := p.VolumeExists(ctx, sourceVolumeID)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to verify source volume %s: %v", sourceVolumeID, err)
		}
		if !exists {
			return status.Errorf(codes.NotFound, "source volume %s not found", sourceVolumeID)
		}
		if !supportsClone {
			return status.Error(codes.Unimplemented, "provider does not support volume cloning")
		}
	default:
		return status.Error(codes.InvalidArgument, "unsupported VolumeContentSource type")
	}
	return nil
}

func validateCreatedVolume(volumeID string, info *provider.VolumeInfo) error {
	if info == nil || strings.TrimSpace(info.Provider) == "" || strings.TrimSpace(info.Path) == "" || info.SizeBytes <= 0 {
		return errors.New("provider returned incomplete cloud volume identity or capacity")
	}
	if info.VolumeID != "" && info.VolumeID != volumeID {
		return fmt.Errorf("provider returned volume identity %q for request %q", info.VolumeID, volumeID)
	}
	return nil
}

func createVolumeResponse(record *volumeRecord, source *csi.VolumeContentSource) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: record.VolumeID, CapacityBytes: record.CapacityBytes,
		VolumeContext: volumeContextForRecord(record), ContentSource: source,
		AccessibleTopology: accessibleTopology(record.Params),
	}}
}

func volumeContextForRecord(rec *volumeRecord) map[string]string {
	volumeCtx := make(map[string]string, len(rec.Params)+len(rec.Metadata)+4)
	for key, value := range rec.Params {
		volumeCtx[key] = value
	}
	for key, value := range rec.Metadata {
		volumeCtx[key] = value
	}
	volumeCtx["cloudProvider"] = rec.Provider
	volumeCtx["cloud-provider"] = rec.Provider
	volumeCtx["cloud-volume-path"] = rec.Path
	volumeCtx["capacity_in_bytes"] = strconv.FormatInt(rec.CapacityBytes, 10)
	return volumeCtx
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}

	unlock, err := cs.locks.lock(ctx, volumeID)
	if err != nil {
		return nil, status.FromContextError(err).Err()
	}
	defer unlock()

	record, loadErr := cs.loadVolumeRecord(ctx, volumeID)
	var params map[string]string
	if loadErr == nil {
		params = record.Params
	} else if errors.Is(loadErr, os.ErrNotExist) {
		params = sanitizePersistableParams(cs.resolveProviderParams(req.GetSecrets()))
		if params == nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s not found in local store and no cloud provider params available for delete; "+
					"configure CSI_CLOUD_PROVIDER (and provider params) or CSI_BOOTSTRAP_PARAMS_FILE so the cloud disk can be deleted",
				volumeID)
		}
		csLogger.Printf("DeleteVolume: volume %s missing from store, deleting via bootstrap params", volumeID)
	} else {
		return nil, status.Errorf(codes.Internal, "failed to load controller state for volume %s: %v", volumeID, loadErr)
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider for delete: %v", err)
	}
	if record != nil && record.State == volumeRecordStatePending {
		creator, ok := p.(provider.OperationVolumeCreator)
		if !ok {
			return nil, status.Errorf(codes.Aborted,
				"volume %s has a pending create; retry CreateVolume before delete", volumeID)
		}
		info, err := creator.CreateVolumeWithOperation(ctx,
			record.VolumeID, record.PlannedCapacityBytes, record.OperationID)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to complete pending create before delete; operation remains recoverable: %v", err)
		}
		if err := validateCreatedVolume(record.VolumeID, info); err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to complete pending create before delete; operation remains recoverable: %v", err)
		}
		record.State = volumeRecordStateReady
		record.Provider = info.Provider
		record.Path = info.Path
		record.CapacityBytes = info.SizeBytes
		record.Metadata = info.Metadata
		if err := cs.store.Save(record); err != nil {
			return nil, status.Errorf(codes.Internal,
				"pending create completed, but ready state persistence failed; retry before delete: %v", err)
		}
	}

	if err := p.DeleteVolume(ctx, volumeID); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "attached") || strings.Contains(errMsg, "in use") || strings.Contains(errMsg, "InUse") {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is still attached to an instance: %v", volumeID, err)
		}
		return nil, status.Errorf(codes.Internal, "provider.DeleteVolume failed: %v", err)
	}

	if err := cs.store.Delete(volumeID); err != nil {
		return nil, status.Errorf(codes.Internal,
			"cloud volume %s was deleted, but controller state cleanup failed: %v", volumeID, err)
	}
	csLogger.Printf("DeleteVolume: %s deleted", volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	if requiredBytes == 0 {
		return nil, status.Error(codes.InvalidArgument, "Required capacity missing")
	}

	rec, err := cs.loadVolumeRecord(ctx, volumeID)
	if err != nil {
		return nil, volumeLookupStatus("volume", volumeID, err)
	}

	if rec.CapacityBytes >= requiredBytes {
		csLogger.Printf("ControllerExpandVolume: %s already at capacity %d >= %d", volumeID, rec.CapacityBytes, requiredBytes)
		return &csi.ControllerExpandVolumeResponse{
			CapacityBytes:         rec.CapacityBytes,
			NodeExpansionRequired: true,
		}, nil
	}

	p, err := provider.NewBlockVolumeProvider(rec.Params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider for expand: %v", err)
	}

	if expander, ok := p.(provider.VolumeExpander); ok {
		if err := expander.ExpandVolume(ctx, volumeID, requiredBytes); err != nil {
			return nil, status.Errorf(codes.Internal, "provider.ExpandVolume failed: %v", err)
		}
	} else {
		return nil, status.Errorf(codes.Unimplemented, "provider %s does not support volume expansion", rec.Provider)
	}

	rec.CapacityBytes = requiredBytes
	if err := cs.store.Save(rec); err != nil {
		return nil, status.Errorf(codes.Internal, "volume %s expanded in cloud but failed to persist record: %v", volumeID, err)
	}

	csLogger.Printf("ControllerExpandVolume: %s expanded to %d bytes", volumeID, requiredBytes)
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         requiredBytes,
		NodeExpansionRequired: true,
	}, nil
}

func (cs *controllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (cs *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	sourceVolumeID := req.GetSourceVolumeId()
	if sourceVolumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Source volume ID missing")
	}
	snapshotName := req.GetName()
	if snapshotName == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot name missing")
	}

	rec, err := cs.loadVolumeRecord(ctx, sourceVolumeID)
	if err != nil {
		return nil, volumeLookupStatus("source volume", sourceVolumeID, err)
	}

	p, err := provider.NewBlockVolumeProvider(rec.Params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	snapshotter, ok := p.(provider.VolumeSnapshotter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "provider %s does not support snapshots", rec.Provider)
	}

	// Targeted lookup by name to enforce uniqueness per CSI spec without listing all snapshots.
	existing, err := snapshotter.FindSnapshot(ctx, snapshotName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check existing snapshot %s: %v", snapshotName, err)
	}
	if existing != nil {
		if existing.SourceVolumeID != sourceVolumeID {
			return nil, status.Errorf(codes.AlreadyExists,
				"snapshot %s already exists for different source volume %s", snapshotName, existing.SourceVolumeID)
		}
		csLogger.Printf("CreateSnapshot: %s already exists, returning existing", snapshotName)
		return &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SnapshotId:     existing.SnapshotID,
				SourceVolumeId: existing.SourceVolumeID,
				SizeBytes:      existing.SizeBytes,
				CreationTime:   timestamppb.New(time.Unix(existing.CreationTime, 0)),
				ReadyToUse:     existing.ReadyToUse,
			},
		}, nil
	}

	snapInfo, err := snapshotter.CreateSnapshot(ctx, sourceVolumeID, snapshotName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.CreateSnapshot failed: %v", err)
	}

	csLogger.Printf("CreateSnapshot: %s from %s", snapshotName, sourceVolumeID)
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snapInfo.SnapshotID,
			SourceVolumeId: sourceVolumeID,
			SizeBytes:      snapInfo.SizeBytes,
			CreationTime:   timestamppb.New(time.Unix(snapInfo.CreationTime, 0)),
			ReadyToUse:     snapInfo.ReadyToUse,
		},
	}, nil
}

func (cs *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	snapshotID := req.GetSnapshotId()
	if snapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID missing")
	}

	params := cs.resolveProviderParams(req.GetSecrets())
	if params == nil {
		csLogger.Printf("DeleteSnapshot: no bootstrap params available for snapshot %s", snapshotID)
		return nil, status.Errorf(codes.FailedPrecondition,
			"cannot determine provider for snapshot %s: configure secrets, CSI_CLOUD_PROVIDER, or CSI_BOOTSTRAP_PARAMS_FILE", snapshotID)
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	snapshotter, ok := p.(provider.VolumeSnapshotter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "provider does not support snapshots")
	}

	if err := snapshotter.DeleteSnapshot(ctx, snapshotID); err != nil {
		return nil, status.Errorf(codes.Internal, "provider.DeleteSnapshot failed: %v", err)
	}

	csLogger.Printf("DeleteSnapshot: %s deleted", snapshotID)
	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *controllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	sourceVolumeID := req.GetSourceVolumeId()
	snapshotID := req.GetSnapshotId()

	var params map[string]string
	if sourceVolumeID != "" {
		rec, err := cs.loadVolumeRecord(ctx, sourceVolumeID)
		if err != nil {
			return nil, volumeLookupStatus("source volume", sourceVolumeID, err)
		}
		params = rec.Params
	} else {
		params = cs.store.BootstrapParams()
	}

	if params == nil {
		return &csi.ListSnapshotsResponse{}, nil
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	snapshotter, ok := p.(provider.VolumeSnapshotter)
	if !ok {
		return &csi.ListSnapshotsResponse{}, nil
	}

	// If a specific snapshot ID is requested, do a targeted lookup.
	if snapshotID != "" {
		snap, err := snapshotter.FindSnapshot(ctx, snapshotID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "provider.FindSnapshot failed: %v", err)
		}
		if snap == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}
		if sourceVolumeID != "" && snap.SourceVolumeID != sourceVolumeID {
			return &csi.ListSnapshotsResponse{}, nil
		}
		return &csi.ListSnapshotsResponse{
			Entries: []*csi.ListSnapshotsResponse_Entry{{
				Snapshot: &csi.Snapshot{
					SnapshotId:     snap.SnapshotID,
					SourceVolumeId: snap.SourceVolumeID,
					SizeBytes:      snap.SizeBytes,
					CreationTime:   timestamppb.New(time.Unix(snap.CreationTime, 0)),
					ReadyToUse:     snap.ReadyToUse,
				},
			}},
		}, nil
	}

	snapInfos, err := snapshotter.ListSnapshots(ctx, sourceVolumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.ListSnapshots failed: %v", err)
	}

	maxEntries := int(req.GetMaxEntries())
	startIdx := 0
	if token := req.GetStartingToken(); token != "" {
		idx, err := strconv.Atoi(token)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid starting_token %q: must be a numeric index", token)
		}
		startIdx = idx
	}

	var entries []*csi.ListSnapshotsResponse_Entry
	for i := startIdx; i < len(snapInfos); i++ {
		if maxEntries > 0 && len(entries) >= maxEntries {
			return &csi.ListSnapshotsResponse{
				Entries:   entries,
				NextToken: strconv.Itoa(i),
			}, nil
		}
		s := snapInfos[i]
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{
			Snapshot: &csi.Snapshot{
				SnapshotId:     s.SnapshotID,
				SourceVolumeId: s.SourceVolumeID,
				SizeBytes:      s.SizeBytes,
				CreationTime:   timestamppb.New(time.Unix(s.CreationTime, 0)),
				ReadyToUse:     s.ReadyToUse,
			},
		})
	}

	return &csi.ListSnapshotsResponse{Entries: entries}, nil
}

var supportedAccessModes = map[csi.VolumeCapability_AccessMode_Mode]bool{
	csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:        true,
	csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:   true,
	csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER: true,
	csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:  true,
}

func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	for _, cap := range caps {
		if cap.GetBlock() != nil {
			return fmt.Errorf("raw block volumes are not supported")
		}
		if cap.GetAccessMode() == nil {
			return fmt.Errorf("access mode is required")
		}
		mode := cap.GetAccessMode().GetMode()
		if !supportedAccessModes[mode] {
			return fmt.Errorf("access mode %s is not supported (only SINGLE_NODE modes are supported for block volumes)", mode)
		}
	}
	return nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing")
	}

	if _, err := cs.loadVolumeRecord(ctx, req.GetVolumeId()); err != nil {
		return nil, volumeLookupStatus("volume", req.GetVolumeId(), err)
	}

	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: err.Error(),
		}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}
