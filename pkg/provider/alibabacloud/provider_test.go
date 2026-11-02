// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	"github.com/alibabacloud-go/tea/tea"
)

func TestValidation(t *testing.T) {
	if _, err := NewAlibabaCloudProvider(nil); err == nil {
		t.Fatal("missing region accepted")
	}
	for _, tc := range []struct {
		bytes int64
		gib   int32
	}{{0, 1}, {gib, 1}, {gib + 1, 2}, {65536 * gib, 65536}} {
		if got, err := bytesToGiB(tc.bytes); err != nil || got != tc.gib {
			t.Fatalf("bytesToGiB(%d) = %d, %v", tc.bytes, got, err)
		}
	}
	for _, size := range []int64{-1, 65536*gib + 1} {
		if _, err := bytesToGiB(size); err == nil {
			t.Fatalf("invalid capacity %d accepted", size)
		}
	}
}

func TestVolumeLifecycle(t *testing.T) {
	exists, creates, denyLookup := false, 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("x-acs-action") {
		case "CreateDisk":
			if r.Form.Get("Size") != "20" || r.Form.Get("PerformanceLevel") != "PL0" || r.Form.Get("Tag.1.Value") != "pvc-test" {
				t.Errorf("unexpected create parameters: %v", r.Form)
			}
			exists, creates = true, creates+1
			_, _ = w.Write([]byte(`{"DiskId":"d-test"}`))
		case "DescribeDisks":
			if denyLookup {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"Code":"Forbidden","Message":"denied"}`))
				return
			}
			disks := []map[string]any{}
			if exists {
				disks = append(disks, map[string]any{"DiskId": "d-test", "Status": "Available", "Size": 20,
					"Tags": map[string]any{"Tag": []map[string]string{{"TagKey": volumeTagKey, "TagValue": "pvc-test"}}}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"TotalCount": len(disks), "Disks": map[string]any{"Disk": disks}})
		case "DeleteDisk":
			if r.Form.Get("DiskId") != "d-test" {
				t.Error("wrong disk ID")
			}
			exists = false
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected API operation %s", r.Header.Get("x-acs-action"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	p := &AlibabaCloudProvider{config: Config{Region: "cn-beijing", Zone: "cn-beijing-i"}, sdk: openapi.Config{
		Endpoint: tea.String(strings.TrimPrefix(server.URL, "http://")), Protocol: tea.String("http"),
		AccessKeyId: tea.String("test-id"), AccessKeySecret: tea.String("test-secret"),
	}}
	ctx := context.Background()
	info, err := p.CreateVolume(ctx, "pvc-test", 20*gib)
	if err != nil || info.Path != "d-test" || info.Metadata["cloud-volume-path"] != "d-test" {
		t.Fatalf("create: %v, %v", info, err)
	}
	// The unchanged node server stages by calling CreateVolume with a 1 GiB default.
	if _, err := p.CreateVolume(ctx, "pvc-test", gib); err != nil || creates != 1 {
		t.Fatalf("stage/replay created another disk: %d, %v", creates, err)
	}
	if info, err := p.GetVolumeInfo(ctx, "pvc-test"); err != nil || info.SizeBytes != 20*gib {
		t.Fatalf("lookup: %v, %v", info, err)
	}
	if found, err := p.VolumeExists(ctx, "pvc-test"); err != nil || !found {
		t.Fatalf("exists: %t, %v", found, err)
	}
	if volumes, err := p.ListManagedVolumes(ctx); err != nil || len(volumes) != 1 || volumes[0].VolumeID != "pvc-test" {
		t.Fatalf("recovery: %v, %v", volumes, err)
	}
	denyLookup = true
	if err := p.DeleteVolume(ctx, "pvc-test"); err == nil {
		t.Fatal("lookup error treated as successful deletion")
	}
	denyLookup = false
	for range 2 {
		if err := p.DeleteVolume(ctx, "pvc-test"); err != nil {
			t.Fatal(err)
		}
	}
}
