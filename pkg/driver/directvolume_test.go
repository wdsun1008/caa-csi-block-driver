// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	b64 "encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDirectVolumeMetadataV1GoldenContract(t *testing.T) {
	metadata, err := readDirectVolumeMetadata(filepath.Join("testdata", "direct-volume-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version != directVolumeMetadataVersion || metadata.Device != "d-workspace123" || !metadata.ReadOnly {
		t.Fatalf("unexpected v1 metadata: %+v", metadata)
	}
	if metadata.FSGroup != "2000" {
		t.Fatalf("unexpected fsGroup: %q", metadata.FSGroup)
	}
	if len(metadata.Options) != 2 || metadata.Options[0] != "noatime" || metadata.Options[1] != "ro" {
		t.Fatalf("unexpected normalized options: %v", metadata.Options)
	}
}

func validDirectVolumeMetadata() directVolumeMetadata {
	return directVolumeMetadata{
		Version:      directVolumeMetadataVersion,
		VolumeType:   "block",
		VolumeMode:   directVolumeModeFilesystem,
		Device:       "cloud-disk-123",
		FsType:       "ext4",
		ReadOnly:     true,
		Options:      []string{"noatime", "ro"},
		FSGroup:      "2000",
		PodUID:       "pod-uid-123",
		VolumeHandle: "pvc-workspace",
		Metadata: map[string]string{
			"cloud-provider":    "testcloud",
			"cloud-volume-path": "cloud-disk-123",
		},
	}
}

func TestWriteDirectVolumeMetadataAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct-volume", mountInfoFileName)
	metadata := validDirectVolumeMetadata()
	if err := writeDirectVolumeMetadataAtomic(path, metadata); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata permissions = %o, want 600", info.Mode().Perm())
	}
	written, err := readDirectVolumeMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if !directVolumeMetadataEqual(*written, metadata) {
		t.Fatalf("metadata changed during atomic write: %+v", written)
	}
	temporary, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".mountInfo.json.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary metadata files remain: %v", temporary)
	}
}

func TestDirectVolumeMetadataRequiresCompleteCDHReference(t *testing.T) {
	metadata := validDirectVolumeMetadata()
	metadata.Metadata["encrypt-type"] = "luks2"
	if err := metadata.validate(); err == nil {
		t.Fatal("incomplete CDH reference was accepted")
	}
	metadata.Metadata["kbs-key-id"] = "default/luks-key/workspace"
	if err := metadata.validate(); err != nil {
		t.Fatalf("complete CDH reference was rejected: %v", err)
	}
}

func publishRequest(target string, mode csi.VolumeCapability_AccessMode_Mode, readonly bool, flags ...string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:   "pvc-workspace",
		TargetPath: target,
		Readonly:   readonly,
		VolumeContext: map[string]string{
			"cloudProvider":              "testcloud",
			"cloud-volume-path":          "cloud-disk-123",
			"csi.storage.k8s.io/pod.uid": "pod-uid-123",
			"cloudAccessKeySecret":       "must-not-leak",
			"cloudSecurityToken":         "must-not-leak",
			"custom-workload-context":    "must-not-cross-boundary",
			"encrypt-type":               "luks2",
			"kbs-key-id":                 "default/luks-key/workspace",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{
				FsType:           "ext4",
				MountFlags:       flags,
				VolumeMountGroup: "2000",
			}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
		},
	}
}

func TestNodePublishVolumeRejectsConflictingMountOptions(t *testing.T) {
	for _, flags := range [][]string{
		{"ro", "rw"}, {"sync", "async"}, {"atime", "noatime"},
		{"diratime", "nodiratime"}, {"exec", "noexec"},
		{"dev", "nodev"}, {"suid", "nosuid"},
	} {
		node, target, _ := setupPublishedNode(t)
		_, err := node.NodePublishVolume(t.Context(), publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false, flags...))
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("flags %v got %v, want InvalidArgument", flags, err)
		}
	}
}

func TestNodeUnpublishReturnsCleanupFailure(t *testing.T) {
	node, target, _ := setupPublishedNode(t)
	original := removeAllPath
	t.Cleanup(func() { removeAllPath = original })
	removeAllPath = func(string) error { return errors.New("injected cleanup failure") }

	_, err := node.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "pvc-workspace", TargetPath: target,
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v, want Internal", err)
	}
}

func setupPublishedNode(t *testing.T) (*nodeServer, string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("KATA_DIRECT_VOLUME_ROOT_PATH", root)
	kubeletRoot := t.TempDir()
	t.Setenv("KUBELET_ROOT_PATH", kubeletRoot)
	target := filepath.Join(kubeletRoot, "pods", "pod-uid-123", "volumes", "kubernetes.io~csi", "pvc-workspace", "mount")
	node := newNodeServer("node-1")
	node.versionedMetadata = true
	node.guestVolumeMountGroup = true
	node.devices["pvc-workspace"] = "cloud-disk-123"
	metadataPath := filepath.Join(root, b64.URLEncoding.EncodeToString([]byte(target)), mountInfoFileName)
	return node, target, metadataPath
}

func TestNodePublishAndUnpublishRejectNonKubeletTargets(t *testing.T) {
	node, _, _ := setupPublishedNode(t)
	outside := filepath.Join(t.TempDir(), "pods", "pod-uid-123", "volumes", "kubernetes.io~csi", "pvc-workspace", "mount")

	_, err := node.NodePublishVolume(t.Context(), publishRequest(outside, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("publish got %v, want InvalidArgument", err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = node.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "pvc-workspace", TargetPath: outside,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unpublish got %v, want InvalidArgument", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unpublish touched an out-of-scope path: %v", err)
	}
}

func TestCSISanityTargetPathRequiresExplicitOptIn(t *testing.T) {
	kubeletRoot := t.TempDir()
	t.Setenv("KUBELET_ROOT_PATH", kubeletRoot)
	target := filepath.Join(kubeletRoot, "pods", "pod-uid-123", "volumes", "kubernetes.io~csi", "pvc-workspace", "target")

	if _, valid := podUIDFromTargetPath(target); valid {
		t.Fatal("csi-sanity target path must be rejected by the production validator")
	}
	t.Setenv(csiSanityTargetPathEnv, "1")
	podUID, valid := podUIDFromTargetPath(target)
	if !valid || podUID != "pod-uid-123" {
		t.Fatalf("csi-sanity target path adapter failed: podUID=%q valid=%t", podUID, valid)
	}
}

func TestNodePublishRejectsPodUIDMismatch(t *testing.T) {
	node, target, _ := setupPublishedNode(t)
	request := publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)
	request.VolumeContext["csi.storage.k8s.io/pod.uid"] = "different-pod"

	_, err := node.NodePublishVolume(t.Context(), request)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodePublishRejectsNonBlockVolumeType(t *testing.T) {
	node, target, _ := setupPublishedNode(t)
	request := publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)
	request.VolumeContext["volume-type"] = "filesystem"

	_, err := node.NodePublishVolume(t.Context(), request)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeUnpublishRejectsVolumeHandleMismatch(t *testing.T) {
	node, target, metadataPath := setupPublishedNode(t)
	if _, err := node.NodePublishVolume(t.Context(), publishRequest(target,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)); err != nil {
		t.Fatal(err)
	}

	_, err := node.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "different-volume", TargetPath: target,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("mismatched unpublish removed metadata: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("mismatched unpublish removed target: %v", err)
	}
}

func TestNodeUnpublishRejectsPodUIDMismatch(t *testing.T) {
	node, target, metadataPath := setupPublishedNode(t)
	if _, err := node.NodePublishVolume(t.Context(), publishRequest(target,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)); err != nil {
		t.Fatal(err)
	}
	metadata, err := readDirectVolumeMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata.PodUID = "different-pod"
	if err := writeDirectVolumeMetadataAtomic(metadataPath, *metadata); err != nil {
		t.Fatal(err)
	}

	_, err = node.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "pvc-workspace", TargetPath: target,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("mismatched unpublish removed metadata: %v", err)
	}
}

func TestNodePublishVolumeReadOnlyContract(t *testing.T) {
	tests := []struct {
		name     string
		mode     csi.VolumeCapability_AccessMode_Mode
		readonly bool
		flags    []string
	}{
		{name: "explicit readonly", mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, readonly: true},
		{name: "reader-only access mode", mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY},
		{name: "multi-node reader-only access mode", mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
		{name: "ro mount flag", mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, flags: []string{"noatime", "ro"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, target, metadataPath := setupPublishedNode(t)
			if _, err := node.NodePublishVolume(t.Context(), publishRequest(target, test.mode, test.readonly, test.flags...)); err != nil {
				t.Fatal(err)
			}
			metadata, err := readDirectVolumeMetadata(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Version != directVolumeMetadataVersion || !metadata.ReadOnly {
				t.Fatalf("unexpected metadata: %#v", metadata)
			}
			if !containsOption(metadata.Options, "ro") || containsOption(metadata.Options, "rw") {
				t.Fatalf("readonly options not normalized: %v", metadata.Options)
			}
			if _, ok := metadata.Metadata["cloudAccessKeySecret"]; ok {
				t.Fatal("access key secret leaked into direct-volume metadata")
			}
			if _, ok := metadata.Metadata["cloudSecurityToken"]; ok {
				t.Fatal("security token leaked into direct-volume metadata")
			}
			if _, ok := metadata.Metadata["custom-workload-context"]; ok {
				t.Fatal("unconsumed volume context crossed into direct-volume metadata")
			}
			if metadata.Metadata["cloud-provider"] != "testcloud" ||
				metadata.Metadata["cloud-volume-path"] != "cloud-disk-123" ||
				metadata.Metadata["encrypt-type"] != "luks2" ||
				metadata.Metadata["kbs-key-id"] != "default/luks-key/workspace" ||
				len(metadata.Metadata) != 4 {
				t.Fatalf("unexpected direct-volume metadata projection: %v", metadata.Metadata)
			}
		})
	}
}

func TestNodePublishVolumeRecoversAfterPluginRestart(t *testing.T) {
	node, target, metadataPath := setupPublishedNode(t)
	delete(node.devices, "pvc-workspace")

	if _, err := node.NodePublishVolume(t.Context(), publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)); err != nil {
		t.Fatal(err)
	}
	metadata, err := readDirectVolumeMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Device != "cloud-disk-123" || node.devices["pvc-workspace"] != "cloud-disk-123" {
		t.Fatalf("plugin state was not recovered: metadata=%+v devices=%v", metadata, node.devices)
	}
}

func TestNodePublishVolumeEnforcesCompatibleRepublish(t *testing.T) {
	node, target, metadataPath := setupPublishedNode(t)
	if _, err := node.NodePublishVolume(t.Context(), publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := node.NodePublishVolume(t.Context(), publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true)); err != nil {
		t.Fatalf("idempotent publish failed: %v", err)
	}
	_, err := node.NodePublishVolume(t.Context(), publishRequest(target, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("got %v, want AlreadyExists", err)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(metadataPath), ".mountInfo.json.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary metadata files remain: %v", matches)
	}
}

func TestNodeUnpublishVolumeIsIdempotent(t *testing.T) {
	node, target, metadataPath := setupPublishedNode(t)
	if _, err := node.NodePublishVolume(t.Context(), publishRequest(target,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)); err != nil {
		t.Fatal(err)
	}
	request := &csi.NodeUnpublishVolumeRequest{VolumeId: "pvc-workspace", TargetPath: target}
	if _, err := node.NodeUnpublishVolume(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := node.NodeUnpublishVolume(t.Context(), request); err != nil {
		t.Fatalf("idempotent unpublish retry failed: %v", err)
	}
	if _, err := os.Stat(metadataPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata remains after unpublish: %v", err)
	}
}

func TestNodeStageUsesOnlyControllerVolumeIdentity(t *testing.T) {
	node := newNodeServer("node-1")
	node.versionedMetadata = true
	node.guestVolumeMountGroup = true
	capability := publishRequest("/unused", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false).VolumeCapability
	request := &csi.NodeStageVolumeRequest{
		VolumeId: "workspace", StagingTargetPath: "/staging/workspace",
		VolumeCapability: capability,
		VolumeContext:    map[string]string{"cloud-volume-path": "d-workspace"},
	}
	if _, err := node.NodeStageVolume(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := node.NodeStageVolume(t.Context(), request); err != nil {
		t.Fatalf("idempotent stage: %v", err)
	}
	changed := *request
	changed.VolumeContext = map[string]string{"cloud-volume-path": "d-other"}
	if _, err := node.NodeStageVolume(t.Context(), &changed); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("changed identity status=%v, want FailedPrecondition", status.Code(err))
	}
}

func TestNodeStageRequiresControllerVolumeIdentity(t *testing.T) {
	node := newNodeServer("node-1")
	node.versionedMetadata = true
	node.guestVolumeMountGroup = true
	capability := publishRequest("/unused", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false).VolumeCapability
	_, err := node.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId: "workspace", StagingTargetPath: "/staging/workspace",
		VolumeCapability: capability,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status=%v, want FailedPrecondition", status.Code(err))
	}
}

func TestReadDirectVolumeMetadataRejectsUnknownAndTrailingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), mountInfoFileName)
	base := `{"version":1,"volume-type":"block","volume-mode":"filesystem","device":"d-1","fstype":"ext4","readonly":true,"options":["noatime","ro"],"pod-uid":"pod-1","volume-handle":"pvc-1"}`
	for name, document := range map[string]string{
		"unknown field":  strings.TrimSuffix(base, "}") + `,"future-field":true}`,
		"trailing value": base + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readDirectVolumeMetadata(path); err == nil {
				t.Fatalf("document was accepted: %s", document)
			}
		})
	}
}

func TestReadDirectVolumeMetadataRejectsOversizedDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), mountInfoFileName)
	if err := os.WriteFile(path, make([]byte, maxDirectVolumeMetadataSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDirectVolumeMetadata(path); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized metadata was not rejected: %v", err)
	}
}

func TestReadDirectVolumeMetadataV1ReadOnlyPresence(t *testing.T) {
	base := `{"version":1,"volume-type":"block","volume-mode":"filesystem","device":"cloud-disk-123","fstype":"ext4","pod-uid":"pod-uid-123","volume-handle":"pvc-workspace"`
	tests := []struct {
		name     string
		readonly string
		want     bool
		wantErr  string
	}{
		{name: "explicit false", readonly: `,"readonly":false}`, want: false},
		{name: "missing", readonly: `}`, wantErr: "explicit readonly"},
		{name: "null", readonly: `,"readonly":null}`, wantErr: "explicit readonly"},
		{name: "wrong type", readonly: `,"readonly":"false"}`, wantErr: "cannot unmarshal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), mountInfoFileName)
			if err := os.WriteFile(path, []byte(base+test.readonly), 0o600); err != nil {
				t.Fatal(err)
			}
			metadata, err := readDirectVolumeMetadata(path)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if metadata.ReadOnly != test.want {
				t.Fatalf("readonly = %t, want %t", metadata.ReadOnly, test.want)
			}
		})
	}
}

func containsOption(options []string, wanted string) bool {
	for _, option := range options {
		if option == wanted {
			return true
		}
	}
	return false
}
