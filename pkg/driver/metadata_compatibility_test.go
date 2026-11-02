// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLegacyPublicationAndFormatMigration(t *testing.T) {
	node, target, path := setupPublishedNode(t)
	node.versionedMetadata, node.guestVolumeMountGroup = false, false
	req := publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)
	req.VolumeCapability.GetMount().VolumeMountGroup = ""
	for i := 0; i < 2; i++ {
		if _, err := node.NodePublishVolume(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "readonly", "fs-group", "volume-handle"} {
		if _, found := legacy[key]; found {
			t.Fatalf("legacy metadata contains %s", key)
		}
	}
	if string(legacy["device"]) != `"cloud-disk-123"` {
		t.Fatalf("wrong device: %s", data)
	}
	req.Readonly = true
	if _, err := node.NodePublishVolume(t.Context(), req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("incompatible republish should fail, got %v", err)
	}
	req.Readonly = false
	req.VolumeCapability.GetMount().VolumeMountGroup = "1000"
	if _, err := node.NodePublishVolume(t.Context(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unsupported fsGroup should fail, got %v", err)
	}
	req.VolumeCapability.GetMount().VolumeMountGroup = ""
	node.versionedMetadata = true
	if _, err := node.NodePublishVolume(t.Context(), req); err == nil {
		t.Fatal("live legacy->v1 migration must require unpublish")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("failed migration altered live metadata")
	}
	if _, err := node.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{VolumeId: req.VolumeId, TargetPath: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := node.NodePublishVolume(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := readDirectVolumeMetadata(path); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyReplaysMinimumProfileRecordWithoutRewriting(t *testing.T) {
	node, target, path := setupPublishedNode(t)
	node.versionedMetadata, node.guestVolumeMountGroup = false, false
	req := publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)
	req.VolumeCapability.GetMount().VolumeMountGroup = ""
	metadata := make(map[string]string)
	for key, value := range req.VolumeContext {
		metadata[key] = value
	}
	metadata["cloud-volume-id"] = req.VolumeId
	old, err := json.Marshal(legacyMountInfo{VolumeType: "directvol", Device: "cloud-disk-123", FsType: "ext4", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMountInfoAtomic(path, old); err != nil {
		t.Fatal(err)
	}
	if _, err := node.NodePublishVolume(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(old) {
		t.Fatal("compatible upgrade rewrote a live legacy record")
	}
}

func TestMountGroupCapabilityRequiresExplicitGuestSupport(t *testing.T) {
	for _, tc := range []struct{ versioned, group, advertised bool }{
		{false, false, false}, {false, true, false}, {true, false, false}, {true, true, true},
	} {
		node := &nodeServer{versionedMetadata: tc.versioned, guestVolumeMountGroup: tc.group}
		result, err := node.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, cap := range result.Capabilities {
			if cap.GetRpc().GetType() == csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP {
				found = true
			}
		}
		if found != tc.advertised {
			t.Fatalf("configuration %+v advertises group=%v", tc, found)
		}
	}
}
