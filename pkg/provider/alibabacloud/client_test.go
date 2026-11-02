// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
)

func TestSDKRequestsHonorCallerCancellation(t *testing.T) {
	for _, operation := range []string{"create", "describe", "delete"} {
		t.Run(operation, func(t *testing.T) {
			received := make(chan struct{})
			aborted := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(received)
				<-r.Context().Done()
				close(aborted)
			}))
			defer server.Close()
			client := &sdkDiskClient{config: &openapi.Config{
				Endpoint: dara.String(strings.TrimPrefix(server.URL, "http://")),
				Protocol: dara.String("http"), RegionId: dara.String("cn-beijing"),
				AccessKeyId: dara.String("synthetic-id"), AccessKeySecret: dara.String("synthetic-secret"),
			}, http: server.Client()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "create":
					_, err = client.CreateDisk(ctx, &ecs.CreateDiskRequest{RegionId: dara.String("cn-beijing"), ZoneId: dara.String("cn-beijing-i"), Size: dara.Int32(20)})
				case "describe":
					_, err = client.DescribeDisks(ctx, &ecs.DescribeDisksRequest{RegionId: dara.String("cn-beijing")})
				case "delete":
					_, err = client.DeleteDisk(ctx, &ecs.DeleteDiskRequest{DiskId: dara.String("d-test")})
				}
				result <- err
			}()
			select {
			case <-received:
			case err := <-result:
				t.Fatalf("SDK did not send the request: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("SDK request did not reach the server")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected caller cancellation, got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SDK call ignored cancellation")
			}
			select {
			case <-aborted:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP request continued after the caller was canceled")
			}
		})
	}
}

func TestOnlyExplicitDiskAbsenceCompletesDelete(t *testing.T) {
	for _, code := range []string{"InvalidRegionId.NotFound", "InvalidAccount.NotFound", "Forbidden", ""} {
		err := &dara.SDKError{Code: dara.String(code), StatusCode: dara.Int(404)}
		if isNotFound(err) {
			t.Fatalf("%q does not prove disk absence", code)
		}
	}
	if !isNotFound(&dara.SDKError{Code: dara.String("InvalidDiskId.NotFound"), StatusCode: dara.Int(400)}) {
		t.Fatal("explicit disk absence must permit idempotent deletion")
	}
}

func TestUnknownCreateResultsKeepTheOperationIdentity(t *testing.T) {
	for _, err := range []error{
		errors.New("invalid character in service response"),
		&dara.SDKError{Code: dara.String("IdempotentProcessing"), StatusCode: dara.Int(400)},
		&dara.SDKError{StatusCode: dara.Int(429)},
		&dara.SDKError{StatusCode: dara.Int(503)},
	} {
		if !isAmbiguousDiskMutationError(err) {
			t.Fatalf("uncertain create incorrectly classified as terminal: %v", err)
		}
	}
}

func TestActualSDKHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code              string
		status            int
		absent, ambiguous bool
	}{
		{"IncorrectDiskStatus.Initializing", 403, false, false},
		{"Forbidden", 404, false, false},
		{"InvalidDiskId.NotFound", 400, true, false},
		{"IdempotentProcessing", 400, false, true},
		{"InternalError", 503, false, true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"Code":%q,"Message":"synthetic service error","RequestId":"test"}`, tc.code)
			}))
			defer server.Close()
			client := &sdkDiskClient{config: &openapi.Config{
				Endpoint: dara.String(strings.TrimPrefix(server.URL, "http://")), Protocol: dara.String("http"), RegionId: dara.String("cn-beijing"),
				AccessKeyId: dara.String("synthetic-id"), AccessKeySecret: dara.String("synthetic-secret"),
			}, http: server.Client()}
			_, err := client.DeleteDisk(t.Context(), &ecs.DeleteDiskRequest{DiskId: dara.String("d-test")})
			if err == nil {
				t.Fatal("HTTP service error was lost")
			}
			code, statusCode, typed := diskSDKError(fmt.Errorf("wrapped: %w", err))
			if !typed || code != tc.code || statusCode != tc.status {
				t.Fatalf("unrecognized SDK response: type=%T code=%q status=%d", err, code, statusCode)
			}
			if isNotFound(err) != tc.absent {
				t.Fatalf("incorrect absence classification for %s", tc.code)
			}
			if isAmbiguousDiskMutationError(err) != tc.ambiguous {
				t.Fatalf("incorrect mutation classification for %s", tc.code)
			}
		})
	}
}
