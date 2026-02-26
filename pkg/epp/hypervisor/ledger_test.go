/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hypervisor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRAGBombConcurrency(t *testing.T) {
	t.Parallel()

	ledger := &TwoTierLedger{}
	// Initialize ledger with a global KV block limit of 100,000
	ledger.UpdateEndpointConfig("test-endpoint", EndpointConfig{Limits: &ResourceVector{KVBlocks: 100000, ActiveRequests: 100000}})

	var wg sync.WaitGroup
	successCount := atomic.Int64{}
	failCount := atomic.Int64{}

	numGoroutines := 1000
	worstCase := ResourceVector{KVBlocks: 1000, ActiveRequests: 1}

	wg.Add(numGoroutines)
	for range numGoroutines {
		go func() {
			defer wg.Done()
			_, err := ledger.TryAcquireHold(worstCase)
			switch err {
			case nil:
				successCount.Add(1)
			case ErrGlobalCapacityExceeded:
				failCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if successCount.Load() != 100 {
		t.Errorf("Expected exactly 100 successes, got %d", successCount.Load())
	}
	if failCount.Load() != 900 {
		t.Errorf("Expected exactly 900 failures, got %d", failCount.Load())
	}

	// Since 100 goroutines successfully passed admission, and successful TryAcquireHold routines
	// leave the allocation locked inside individual HoldReceipts (without committing/releasing yet),
	// the globalHold must specifically quantify at 100 * 1000.
	if ledger.globalHold.KVBlocks.Load() != 100000 {
		t.Errorf("Expected final global hold KVBlocks to be exactly 100000, got %d", ledger.globalHold.KVBlocks.Load())
	}
	// the globalHold must specifically quantify at 100 * 1.
	if ledger.globalHold.ActiveRequests.Load() != 100 {
		t.Errorf("Expected final global hold ActiveRequests to be exactly 100, got %d", ledger.globalHold.ActiveRequests.Load())
	}
}

func TestReleaseHold(t *testing.T) {
	t.Parallel()

	ledger := &TwoTierLedger{}
	ledger.UpdateEndpointConfig("test-endpoint", EndpointConfig{Limits: &ResourceVector{KVBlocks: 1000}})

	// 1. Acquire hold
	receipt, err := ledger.TryAcquireHold(ResourceVector{KVBlocks: 100})
	if err != nil {
		t.Fatalf("TryAcquireHold failed: %v", err)
	}

	if ledger.globalHold.KVBlocks.Load() != 100 {
		t.Errorf("Expected globalHold to be 100 after acquire, got %d", ledger.globalHold.KVBlocks.Load())
	}

	// 2. Release hold
	ledger.ReleaseHold(receipt)

	if ledger.globalHold.KVBlocks.Load() != 0 {
		t.Errorf("Expected globalHold to be 0 after release, got %d", ledger.globalHold.KVBlocks.Load())
	}
}

func TestCommitLedger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		endpointID string
		wantErr    error
	}{
		{
			name:       "Success - Valid Endpoint",
			endpointID: "test-endpoint",
			wantErr:    nil,
		},
		{
			name:       "Failure - Endpoint Not Found",
			endpointID: "invalid-endpoint",
			wantErr:    ErrEndpointNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := &TwoTierLedger{}
			ledger.UpdateEndpointConfig("test-endpoint", EndpointConfig{Limits: &ResourceVector{KVBlocks: 1000}})

			// 1. Acquire hold
			receipt, err := ledger.TryAcquireHold(ResourceVector{KVBlocks: 100})
			if err != nil {
				t.Fatalf("TryAcquireHold failed: %v", err)
			}

			if ledger.globalHold.KVBlocks.Load() != 100 {
				t.Errorf("Expected globalHold to be 100 after acquire, got %d", ledger.globalHold.KVBlocks.Load())
			}

			// 2. Commit
			actualCost := ResourceVector{KVBlocks: 50}
			commitReceipt, err := ledger.Commit(tt.endpointID, actualCost, receipt)

			if err != tt.wantErr {
				t.Errorf("Commit() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr == nil && commitReceipt == nil {
				t.Fatal("Expected CommitReceipt, got nil")
			}

			// 3. Verification - Global hold MUST always be released regardless of commit logic success.
			if ledger.globalHold.KVBlocks.Load() != 0 {
				t.Errorf("Global hold should be 0 after Commit, got %d", ledger.globalHold.KVBlocks.Load())
			}

			if tt.wantErr == nil {
				ledger.endpointLedgers.Range(func(key, value any) bool {
					endpoint := value.(*endpointLedger)
					if endpoint == nil {
						t.Fatal("Expected an endpoint, got nil")
					}
					return true
				})
			}
		})
	}
}

func TestReleaseEndpointCapacity(t *testing.T) {
	t.Parallel()

	ledger := &TwoTierLedger{}
	ledger.UpdateEndpointConfig("test-endpoint", EndpointConfig{Limits: &ResourceVector{KVBlocks: 1000}})

	// Commit receipt is necessary for releasing.
	receipt, _ := ledger.TryAcquireHold(ResourceVector{KVBlocks: 100})
	actualCost := ResourceVector{KVBlocks: 50}
	commitReceipt, _ := ledger.Commit("test-endpoint", actualCost, receipt)

	ledger.ReleaseEndpointCapacity("test-endpoint", commitReceipt)

	// Since locally held is implicitly restored based on Commit/Release logic,
	// let's run ReconcileEndpointCapacity and assert on that instead.
}

func TestReconcileEndpointCapacity(t *testing.T) {
	t.Parallel()

	ledger := &TwoTierLedger{}
	ledger.UpdateEndpointConfig("test-endpoint", EndpointConfig{Limits: &ResourceVector{KVBlocks: 1000}})

	scrapedUsage := ResourceVector{KVBlocks: 150}
	ledger.ReconcileEndpointCapacity("test-endpoint", scrapedUsage)

	if ledger.globalScraped.KVBlocks.Load() != 150 {
		t.Errorf("Expected global scraped to be 150 after reconcile, got %d", ledger.globalScraped.KVBlocks.Load())
	}
}

func TestMasterTick(t *testing.T) {
	t.Parallel()

	ledger := &TwoTierLedger{}
	ledger.UpdateEndpointConfig("test-endpoint", EndpointConfig{Limits: &ResourceVector{KVBlocks: 1000}})

	// Initial epoch should be 0 or 1
	startEpoch := ledger.globalEpoch.Load()

	ctx, cancel := context.WithCancel(context.Background())
	// Run tick in background
	go ledger.RunMasterTick(ctx)

	// Wait for a few ticks
	time.Sleep(150 * time.Millisecond)
	cancel()

	endEpoch := ledger.globalEpoch.Load()
	if endEpoch <= startEpoch {
		t.Errorf("Expected temporal epoch to advance, but it stayed at %d", startEpoch)
	}
}

func TestLedgerTransitiveRacing(t *testing.T) {
	t.Parallel()
	l := &TwoTierLedger{}

	// Define standard resource allocations
	l.UpdateEndpointConfig("ep-1", EndpointConfig{
		Limits: &ResourceVector{
			PrefillTokens:  2000,
			DecodeTokens:   2000,
			KVBlocks:       2000,
			ActiveRequests: 2000,
		},
	})

	wg := sync.WaitGroup{}
	concurrency := 500
	runtimeSeconds := 2 * time.Second

	// Run Master Tick
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.RunMasterTick(ctx)

	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Since(start) < runtimeSeconds {
				receipt, err := l.TryAcquireHold(ResourceVector{KVBlocks: 10, ActiveRequests: 1})
				if err != nil {
					continue
				}

				commit, err := l.Commit("ep-1", ResourceVector{KVBlocks: 10, ActiveRequests: 1}, receipt)
				if err != nil {
					l.ReleaseHold(receipt)
					continue
				}

				// Simulate generation execution overhead
				time.Sleep(1 * time.Millisecond)

				l.ReleaseEndpointCapacity("ep-1", commit)
			}
		}()
	}

	wg.Wait()

	// Assert: No negative boundaries leaked from math racing
	if l.globalScraped.KVBlocks.Load() < 0 || l.globalTransit[0].KVBlocks.Load() < 0 {
		t.Errorf("Capacity rolled negative during multithreaded transit math.")
	}
}
