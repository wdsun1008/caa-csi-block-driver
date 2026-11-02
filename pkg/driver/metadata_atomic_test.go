// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAtomicMetadataFailedRenamePreservesTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountInfo.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "preserve")
	if err := os.WriteFile(marker, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeMountInfoAtomic(path, []byte(`{"device":"disk"}`)); err == nil {
		t.Fatal("rename into directory should fail")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "original" {
		t.Fatalf("target changed: %q, %v", data, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".mountInfo.json.*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v, %v", leftovers, err)
	}
}

func TestAtomicMetadataPublishReplaysAfterNodeRestart(t *testing.T) {
	t.Setenv("KATA_DIRECT_VOLUME_ROOT_PATH", t.TempDir())
	kubelet := t.TempDir()
	t.Setenv("KUBELET_ROOT_PATH", kubelet)
	target := filepath.Join(kubelet, "pods", "pod-uid", "volumes", "kubernetes.io~csi", "volume-1", "mount")
	request := &csi.NodePublishVolumeRequest{
		VolumeId: "volume-1", TargetPath: target,
		VolumeContext:    map[string]string{"cloud-volume-path": "disk-1"},
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}},
	}
	for i := 0; i < 2; i++ {
		if _, err := newNodeServer("node").NodePublishVolume(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	if len(request.VolumeContext) != 1 {
		t.Fatal("publish mutated caller volume context")
	}
	request.VolumeContext["cloud-volume-path"] = "other-disk"
	if _, err := newNodeServer("node").NodePublishVolume(t.Context(), request); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestUnpublishReturnsCleanupFailure(t *testing.T) {
	original := removeAllPath
	removeAllPath = func(string) error { return errors.New("injected failure") }
	defer func() { removeAllPath = original }()
	_, err := newNodeServer("node").NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "volume-1", TargetPath: filepath.Join(getKubeletRootPath(), "pods", "pod-uid", "volumes", "kubernetes.io~csi", "volume-1", "mount"),
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v", err)
	}
}
