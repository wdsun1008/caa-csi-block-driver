// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"errors"
	"testing"

	ecs "github.com/alibabacloud-go/ecs-20140526/v4/client"
	"github.com/alibabacloud-go/tea/tea"
)

const (
	testOperationID  = "0123456789abcdef0123456789abcdef"
	testOperationID2 = "fedcba9876543210fedcba9876543210"
)

type fakeDiskClient struct {
	createRequests   []*ecs.CreateDiskRequest
	deleteRequests   []*ecs.DeleteDiskRequest
	describeRequests []*ecs.DescribeDisksRequest
	createResponse   *ecs.CreateDiskResponse
	createError      error
	describeResults  [][]*ecs.DescribeDisksResponseBodyDisksDisk
}

func (f *fakeDiskClient) CreateDisk(request *ecs.CreateDiskRequest) (*ecs.CreateDiskResponse, error) {
	f.createRequests = append(f.createRequests, request)
	return f.createResponse, f.createError
}

func (f *fakeDiskClient) DeleteDisk(request *ecs.DeleteDiskRequest) (*ecs.DeleteDiskResponse, error) {
	f.deleteRequests = append(f.deleteRequests, request)
	return &ecs.DeleteDiskResponse{}, nil
}

func (f *fakeDiskClient) DescribeDisks(request *ecs.DescribeDisksRequest) (*ecs.DescribeDisksResponse, error) {
	f.describeRequests = append(f.describeRequests, request)
	var disks []*ecs.DescribeDisksResponseBodyDisksDisk
	if len(f.describeResults) > 0 {
		disks = f.describeResults[0]
		f.describeResults = f.describeResults[1:]
	}
	return describeResponse(disks), nil
}

func (f *fakeDiskClient) ResizeDisk(*ecs.ResizeDiskRequest) (*ecs.ResizeDiskResponse, error) {
	return &ecs.ResizeDiskResponse{}, nil
}

func describeResponse(disks []*ecs.DescribeDisksResponseBodyDisksDisk) *ecs.DescribeDisksResponse {
	return &ecs.DescribeDisksResponse{Body: &ecs.DescribeDisksResponseBody{
		Disks: &ecs.DescribeDisksResponseBodyDisks{Disk: disks},
	}}
}

func managedDisk(logicalID, diskID string) *ecs.DescribeDisksResponseBodyDisksDisk {
	return &ecs.DescribeDisksResponseBodyDisksDisk{
		DiskId: tea.String(diskID),
		Size:   tea.Int32(20),
		Status: tea.String("Available"),
		ZoneId: tea.String("cn-hangzhou-h"),
		Type:   tea.String("data"),
		Tags: &ecs.DescribeDisksResponseBodyDisksDiskTags{Tag: []*ecs.DescribeDisksResponseBodyDisksDiskTagsTag{
			{TagKey: tea.String(volumeTagKey), TagValue: tea.String(logicalID)},
			{TagKey: tea.String(driverTagKey), TagValue: tea.String(driverTagValue)},
			{TagKey: tea.String(clusterIDTagKey), TagValue: tea.String("cluster-123")},
			{TagKey: tea.String(durableVolumeTag), TagValue: tea.String("true")},
		}},
	}
}

func compatibleCreateDisk(p *AlibabaCloudProvider, logicalID, diskID string, sizeGiB int64, status string) *ecs.DescribeDisksResponseBodyDisksDisk {
	disk := managedDisk(logicalID, diskID)
	disk.Size = tea.Int32(int32(sizeGiB))
	disk.Status = tea.String(status)
	disk.Type = tea.String("data")
	disk.Category = tea.String(p.config.Category)
	disk.PerformanceLevel = optionalString(p.config.PerformanceLevel)
	disk.ResourceGroupId = optionalString(p.config.ResourceGroupID)
	disk.MultiAttach = tea.String("Disabled")
	disk.Tags = &ecs.DescribeDisksResponseBodyDisksDiskTags{Tag: []*ecs.DescribeDisksResponseBodyDisksDiskTagsTag{
		{TagKey: tea.String(volumeTagKey), TagValue: tea.String(logicalID)},
		{TagKey: tea.String(driverTagKey), TagValue: tea.String(driverTagValue)},
		{TagKey: tea.String(clusterIDTagKey), TagValue: tea.String(p.config.ClusterID)},
		{TagKey: tea.String(durableVolumeTag), TagValue: tea.String("true")},
		{TagKey: tea.String(createSpecTag), TagValue: tea.String(p.createSpecFingerprint(logicalID, sizeGiB))},
		{TagKey: tea.String(createOperationTag), TagValue: tea.String("test-create-operation")},
	}}
	return disk
}

func TestParseConfig(t *testing.T) {
	config, err := parseConfig(map[string]string{
		"region":           "cn-hangzhou",
		"zoneId":           "cn-hangzhou-h",
		"type":             "cloud_essd",
		"performanceLevel": "PL1",
		"clusterId":        "cluster-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Category != "cloud_essd" || config.PerformanceLevel != "PL1" || config.ClusterID != "cluster-123" {
		t.Fatalf("unexpected config: %#v", config)
	}
	if _, err := parseConfig(map[string]string{"region": "cn-hangzhou"}); err == nil {
		t.Fatal("missing zone must fail")
	}
	if _, err := parseConfig(map[string]string{"region": "cn-hangzhou", "zoneId": "cn-hangzhou-h"}); err == nil {
		t.Fatal("missing cluster ID must fail")
	}
}

func TestCreateVolumeUsesOperationScopedToken(t *testing.T) {
	fake := &fakeDiskClient{}
	provider := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd", PerformanceLevel: "PL1", ClusterID: "cluster-123",
	}, fake)
	disk := compatibleCreateDisk(provider, "workspace", "d-workspace123", 20, "Available")
	fake.createResponse = &ecs.CreateDiskResponse{Body: &ecs.CreateDiskResponseBody{DiskId: disk.DiskId}}
	fake.describeResults = [][]*ecs.DescribeDisksResponseBodyDisksDisk{nil, {disk}}
	info, err := provider.CreateVolumeWithOperation("workspace", 2*gib, testOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != "d-workspace123" || info.Metadata["cloud-provider"] != providerName || info.Metadata["cloud-volume-path"] != "d-workspace123" {
		t.Fatalf("unexpected volume info: %#v", info)
	}
	if len(fake.createRequests) != 1 || tea.StringValue(fake.createRequests[0].ClientToken) == "" {
		t.Fatalf("missing client token: %#v", fake.createRequests)
	}
	if tea.Int32Value(fake.createRequests[0].Size) != 20 {
		t.Fatalf("PL1 minimum size was not applied: %d", tea.Int32Value(fake.createRequests[0].Size))
	}
	if got := len(fake.createRequests[0].Tag); got != 6 {
		t.Fatalf("CreateDisk tag count = %d, want cluster-scoped ownership tags", got)
	}
	operationID := ""
	for _, tag := range fake.createRequests[0].Tag {
		if tea.StringValue(tag.Key) == createOperationTag {
			operationID = tea.StringValue(tag.Value)
		}
	}
	if operationID == "" || tea.StringValue(fake.createRequests[0].ClientToken) != "caa-csi-"+operationID {
		t.Fatalf("client token and operation tag are not scoped to one create: %#v", fake.createRequests[0])
	}
}

func TestCreateVolumeKeepsLogicalAndPhysicalDiskNamespacesSeparate(t *testing.T) {
	fake := &fakeDiskClient{}
	p := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd", PerformanceLevel: "PL0",
		ClusterID: "cluster-123",
	}, fake)
	disk := compatibleCreateDisk(p, "d-workspace", "d-physical123", 1, "Available")
	fake.describeResults = [][]*ecs.DescribeDisksResponseBodyDisksDisk{{disk}}

	info, err := p.CreateVolumeWithOperation("d-workspace", gib, testOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if info.VolumeID != "d-workspace" || info.Path != "d-physical123" || len(fake.createRequests) != 0 {
		t.Fatalf("logical d-* adoption = %+v, creates=%d", info, len(fake.createRequests))
	}
	if len(fake.describeRequests) != 1 || fake.describeRequests[0].DiskIds != nil {
		t.Fatalf("logical d-* name was queried as a physical disk ID: %#v", fake.describeRequests)
	}
	if got := tea.StringValue(fake.describeRequests[0].Tag[0].Value); got != "d-workspace" {
		t.Fatalf("logical volume tag query=%q, want d-workspace", got)
	}
}

func TestGetVolumeInfoUsesLogicalIdentity(t *testing.T) {
	disk := managedDisk("d-workspace", "d-physical123")
	fake := &fakeDiskClient{describeResults: [][]*ecs.DescribeDisksResponseBodyDisksDisk{{disk}}}
	p := newAlibabaCloudProviderWithClient(Config{Region: "cn-hangzhou", ClusterID: "cluster-123"}, fake)

	info, err := p.GetVolumeInfo("d-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if info.VolumeID != "d-workspace" || info.Path != "d-physical123" {
		t.Fatalf("external lookup returned %+v", info)
	}
	if len(fake.describeRequests) != 1 || fake.describeRequests[0].DiskIds != nil || len(fake.describeRequests[0].Tag) == 0 {
		t.Fatalf("logical lookup did not use ownership tags: %#v", fake.describeRequests)
	}
}

func TestDeleteVolumeUsesLogicalIdentity(t *testing.T) {
	disk := managedDisk("d-workspace", "d-physical123")
	fake := &fakeDiskClient{describeResults: [][]*ecs.DescribeDisksResponseBodyDisksDisk{{disk}, nil}}
	p := newAlibabaCloudProviderWithClient(Config{Region: "cn-hangzhou", ClusterID: "cluster-123"}, fake)

	if err := p.DeleteVolume("d-workspace"); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleteRequests) != 1 || tea.StringValue(fake.deleteRequests[0].DiskId) != "d-physical123" {
		t.Fatalf("logical recovery deleted %#v", fake.deleteRequests)
	}
	if fake.describeRequests[0].DiskIds != nil || fake.describeRequests[1].DiskIds == nil {
		t.Fatalf("delete did not transition from logical lookup to physical disappearance check: %#v", fake.describeRequests)
	}
}

func TestCreateVolumeReconcilesUnknownSuccess(t *testing.T) {
	fake := &fakeDiskClient{
		createError: errors.New("client timeout after server commit"),
	}
	provider := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd", PerformanceLevel: "PL0",
		ClusterID: "cluster-123",
	}, fake)
	disk := compatibleCreateDisk(provider, "workspace", "d-workspace123", 1, "Available")
	fake.describeResults = [][]*ecs.DescribeDisksResponseBodyDisksDisk{nil, {disk}}
	info, err := provider.CreateVolumeWithOperation("workspace", gib, testOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != "d-workspace123" {
		t.Fatalf("reconciled wrong disk: %#v", info)
	}
}

func TestCreateVolumeWaitsForCompatibleCreatingDisk(t *testing.T) {
	fake := &fakeDiskClient{}
	p := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd",
		PerformanceLevel: "PL1", ClusterID: "cluster-123",
	}, fake)
	creating := compatibleCreateDisk(p, "workspace", "d-workspace123", 20, "Creating")
	// ECS may omit immutable fields while the disk is still being created.
	creating.Type, creating.Category, creating.Size = nil, nil, nil
	ready := compatibleCreateDisk(p, "workspace", "d-workspace123", 20, "In_use")
	ready.MultiAttach = nil // DescribeDisks may omit the default Disabled value.
	fake.describeResults = [][]*ecs.DescribeDisksResponseBodyDisksDisk{{creating}, {ready}}

	info, err := p.CreateVolumeWithOperation("workspace", 2*gib, testOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != "d-workspace123" || len(fake.createRequests) != 0 || len(fake.describeRequests) != 2 {
		t.Fatalf("creating-disk recovery = %#v, creates=%d describes=%d", info, len(fake.createRequests), len(fake.describeRequests))
	}
}

func TestCreateVolumeRejectsStoreLossWithChangedCreateSpec(t *testing.T) {
	oldProvider := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd",
		PerformanceLevel: "PL1", ClusterID: "cluster-123",
	}, &fakeDiskClient{})
	existing := compatibleCreateDisk(oldProvider, "workspace", "d-workspace123", 20, "Available")
	fake := &fakeDiskClient{describeResults: [][]*ecs.DescribeDisksResponseBodyDisksDisk{{existing}}}
	newProvider := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd",
		PerformanceLevel: "PL2", ClusterID: "cluster-123",
	}, fake)

	if _, err := newProvider.CreateVolumeWithOperation("workspace", 2*gib, testOperationID); err == nil {
		t.Fatal("store-loss recovery adopted a disk created with different immutable parameters")
	}
	if len(fake.createRequests) != 0 {
		t.Fatal("incompatible existing disk triggered a second CreateDisk mutation")
	}
}

func TestCreateVolumeAdoptsSameSpecAfterProviderReconstruction(t *testing.T) {
	oldProvider := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd",
		PerformanceLevel: "PL1", ClusterID: "cluster-123",
	}, &fakeDiskClient{})
	existing := compatibleCreateDisk(oldProvider, "workspace", "d-workspace123", 20, "Available")
	fake := &fakeDiskClient{describeResults: [][]*ecs.DescribeDisksResponseBodyDisksDisk{{existing}}}
	newProvider := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd",
		PerformanceLevel: "PL1", ClusterID: "cluster-123",
	}, fake)

	info, err := newProvider.CreateVolumeWithOperation("workspace", 2*gib, testOperationID)
	if err != nil || info.Path != "d-workspace123" {
		t.Fatalf("same-spec recovery = (%+v, %v)", info, err)
	}
	if len(fake.createRequests) != 0 {
		t.Fatal("same immutable spec triggered a second CreateDisk mutation")
	}
}

func TestClientTokenComesOnlyFromPersistedOperation(t *testing.T) {
	config := Config{ClusterID: "cluster-token-test", Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd"}
	firstClient := &fakeDiskClient{createError: &tea.SDKError{StatusCode: tea.Int(400), Code: tea.String("InvalidParameter")}}
	secondClient := &fakeDiskClient{createError: &tea.SDKError{StatusCode: tea.Int(400), Code: tea.String("InvalidParameter")}}
	firstProvider := newAlibabaCloudProviderWithClient(config, firstClient)
	secondProvider := newAlibabaCloudProviderWithClient(config, secondClient)

	if _, err := firstProvider.CreateVolume("workspace-token-test", gib); err == nil {
		t.Fatal("legacy create without a persisted operation ID was accepted")
	}
	if _, err := firstProvider.CreateVolumeWithOperation("workspace-token-test", gib, "invalid"); err == nil {
		t.Fatal("invalid operation ID was accepted")
	}
	_, _ = firstProvider.CreateVolumeWithOperation("workspace-token-test", gib, testOperationID)
	_, _ = secondProvider.CreateVolumeWithOperation("workspace-token-test", gib, testOperationID)
	firstToken := tea.StringValue(firstClient.createRequests[0].ClientToken)
	secondToken := tea.StringValue(secondClient.createRequests[0].ClientToken)
	if firstToken != "caa-csi-"+testOperationID || secondToken != firstToken {
		t.Fatalf("reconstructed provider tokens = (%q, %q), want persisted operation token", firstToken, secondToken)
	}
}

func TestNormalizeCapacityHonorsESSDMinimum(t *testing.T) {
	p := newAlibabaCloudProviderWithClient(Config{Category: "cloud_essd", PerformanceLevel: "PL1"}, &fakeDiskClient{})
	actual, err := p.NormalizeCapacity(2 * gib)
	if err != nil || actual != 20*gib {
		t.Fatalf("NormalizeCapacity = (%d, %v), want (%d, nil)", actual, err, 20*gib)
	}
}

func TestCreateVolumeDoesNotReconcileExplicitFailure(t *testing.T) {
	fake := &fakeDiskClient{
		createError:     &tea.SDKError{StatusCode: tea.Int(400), Code: tea.String("InvalidParameter.DiskCategory")},
		describeResults: [][]*ecs.DescribeDisksResponseBodyDisksDisk{nil, nil},
	}
	p := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", Zone: "cn-hangzhou-h", Category: "cloud_essd", ClusterID: "cluster-123",
	}, fake)
	if _, err := p.CreateVolumeWithOperation("workspace", gib, testOperationID); err == nil {
		t.Fatal("explicit CreateDisk failure was accepted")
	} else if terminal, ok := err.(interface{ TerminalCreateFailure() bool }); !ok || !terminal.TerminalCreateFailure() {
		t.Fatalf("explicit failure was not marked terminal: %T %v", err, err)
	}
	if len(fake.describeRequests) != 1 {
		t.Fatalf("explicit failure made %d DescribeDisks calls, want preflight only", len(fake.describeRequests))
	}
	firstToken := tea.StringValue(fake.createRequests[0].ClientToken)
	if _, err := p.CreateVolumeWithOperation("workspace", gib, testOperationID2); err == nil {
		t.Fatal("second explicit CreateDisk failure was accepted")
	}
	secondToken := tea.StringValue(fake.createRequests[1].ClientToken)
	if firstToken == "" || firstToken == secondToken {
		t.Fatalf("new operation after explicit failure reused ClientToken %q", firstToken)
	}
}

func TestDeleteVolumeRetryMakesOnlyOneEffectiveMutation(t *testing.T) {
	disk := managedDisk("workspace", "d-workspace123")
	fake := &fakeDiskClient{describeResults: [][]*ecs.DescribeDisksResponseBodyDisksDisk{
		{disk},
		nil,
	}}
	p := newAlibabaCloudProviderWithClient(Config{
		Region: "cn-hangzhou", ClusterID: "cluster-123",
	}, fake)

	if err := p.DeleteVolume("workspace"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteVolume("workspace"); err != nil {
		t.Fatalf("idempotent delete retry: %v", err)
	}
	if len(fake.deleteRequests) != 1 {
		t.Fatalf("DeleteDisk calls=%d, want one effective provider mutation", len(fake.deleteRequests))
	}
}

func TestDeleteVolumeRejectsIncompleteDurableOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ecs.DescribeDisksResponseBodyDisksDisk)
	}{
		{name: "missing durable tag", mutate: func(disk *ecs.DescribeDisksResponseBodyDisksDisk) {
			for _, tag := range disk.Tags.Tag {
				if tea.StringValue(tag.TagKey) == durableVolumeTag {
					tag.TagValue = tea.String("false")
				}
			}
		}},
		{name: "missing logical identity", mutate: func(disk *ecs.DescribeDisksResponseBodyDisksDisk) {
			for _, tag := range disk.Tags.Tag {
				if tea.StringValue(tag.TagKey) == volumeTagKey {
					tag.TagValue = tea.String("")
				}
			}
		}},
		{name: "not a data disk", mutate: func(disk *ecs.DescribeDisksResponseBodyDisksDisk) {
			disk.Type = tea.String("system")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			disk := managedDisk("workspace", "d-workspace123")
			test.mutate(disk)
			fake := &fakeDiskClient{describeResults: [][]*ecs.DescribeDisksResponseBodyDisksDisk{{disk}}}
			p := newAlibabaCloudProviderWithClient(Config{Region: "cn-hangzhou", ClusterID: "cluster-123"}, fake)
			if err := p.DeleteVolume("workspace"); err == nil {
				t.Fatal("disk with incomplete ownership was deleted")
			}
			if len(fake.deleteRequests) != 0 {
				t.Fatal("ownership rejection reached DeleteDisk")
			}
		})
	}
}
