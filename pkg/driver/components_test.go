// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeModeNeedsNoControllerState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unusable-store")
	if err := os.WriteFile(path, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DriverName: "test", VendorVersion: "test", NodeID: "node", Component: "node", VolumeStoreDir: path}
	node, err := NewDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if node.store != nil {
		t.Fatal("node initialized controller state")
	}
	server := grpc.NewServer()
	node.registerServices(server)
	services := server.GetServiceInfo()
	if _, found := services["csi.v1.Controller"]; found {
		t.Fatal("node serves controller RPCs")
	}
	if _, found := services["csi.v1.Node"]; !found {
		t.Fatal("node RPCs missing")
	}
	cfg.Component = "controller"
	if _, err := NewDriver(cfg); err == nil {
		t.Fatal("controller accepted an unusable journal directory")
	}
}

func TestNodeStageUsesOnlyControllerIdentity(t *testing.T) {
	node := newNodeServer("node")
	req := &csi.NodeStageVolumeRequest{
		VolumeId: "volume", StagingTargetPath: filepath.Join(t.TempDir(), "stage"),
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}},
		VolumeContext:    map[string]string{"cloud-volume-path": "disk", "cloudProvider": "not-a-registered-provider"},
	}
	if _, err := node.NodeStageVolume(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	req.VolumeContext["cloud-volume-path"] = "replacement"
	if _, err := node.NodeStageVolume(t.Context(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
	delete(req.VolumeContext, "cloud-volume-path")
	if _, err := node.NodeStageVolume(t.Context(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
}
