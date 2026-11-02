// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"errors"
	"testing"
)

func TestCanceledVolumeRequestDoesNotWaitForAnotherMutation(t *testing.T) {
	var locks volumeNameLocks
	first, err := locks.lock(context.Background(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locks.lock(ctx, "workspace"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter should return immediately: %v", err)
	}
	other, err := locks.lock(context.Background(), "other-workspace")
	if err != nil {
		t.Fatal(err)
	}
	other()
	first()
	if len(locks.locks) != 0 {
		t.Fatal("completed/canceled operations left lock entries behind")
	}
}
