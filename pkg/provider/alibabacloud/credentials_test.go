// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aliyun/credentials-go/credentials"
)

type blockedCredential struct {
	credentials.Credential
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (c *blockedCredential) GetCredential() (*credentials.CredentialModel, error) {
	c.calls.Add(1)
	c.entered <- struct{}{}
	<-c.release
	return &credentials.CredentialModel{}, nil
}

func TestCredentialRefreshCancellationIsBoundedAndSerialized(t *testing.T) {
	source := &blockedCredential{entered: make(chan struct{}, 2), release: make(chan struct{})}
	defer close(source.release)
	resolver := newCredentialResolver(source)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := resolver.resolve(ctx); done <- err }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("credential refresh did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller remained blocked in credential SDK")
	}
	// A retry must not start an additional uncancellable SDK refresh.
	retry, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	if _, err := resolver.resolve(retry); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if source.calls.Load() != 1 {
		t.Fatalf("started %d concurrent refreshes", source.calls.Load())
	}
}
