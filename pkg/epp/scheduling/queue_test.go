package scheduling

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1alpha2 "sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

type mockSchedulableRequest struct {
	ctx     context.Context
	request *types.LLMRequest
	size    uint64
}

func (m *mockSchedulableRequest) Context() context.Context {
	return m.ctx
}

func (m *mockSchedulableRequest) Request() *types.LLMRequest {
	return m.request
}

func (m *mockSchedulableRequest) Size() uint64 {
	return m.size
}

// mockClock is a mock implementation of the clock interface for testing.
type mockClock struct {
	currentTime time.Time
}

func (m *mockClock) now() time.Time {
	return m.currentTime
}

type mockScheduler struct {
	scheduleFunc func(ctx context.Context, req *types.LLMRequest) (targetPod types.Pod, err error)
}

func (m *mockScheduler) Schedule(ctx context.Context, req *types.LLMRequest) (types.Pod, error) {
	return m.scheduleFunc(ctx, req)
}

func TestQueueConfig_validateAndApplyDefaults(t *testing.T) {
	tests := []struct {
		name          string
		config        QueueConfig
		expected      QueueConfig
		expectedError bool
	}{
		{
			name: "All Defaults",
			config: QueueConfig{
				TotalQueueCapacity:    0,
				ModelQueueCapacity:    0,
				QueueTTL:              0,
				ExpiryCleanupInterval: 0,
			},
			expected: QueueConfig{
				TotalQueueCapacity:    DefaultTotalQueueCapacity,
				ModelQueueCapacity:    DefaultModelQueueCapacity,
				QueueTTL:              DefaultQueueTTL,
				ExpiryCleanupInterval: DefaultExpiryCleanupInterval,
			},
			expectedError: false,
		},
		{
			name: "Custom Values",
			config: QueueConfig{
				TotalQueueCapacity:    200 * 1024 * 1024, // 200MB
				ModelQueueCapacity:    20 * 1024 * 1024,  // 20MB
				QueueTTL:              60 * time.Second,
				ExpiryCleanupInterval: 2 * time.Second,
			},
			expected: QueueConfig{
				TotalQueueCapacity:    200 * 1024 * 1024,
				ModelQueueCapacity:    20 * 1024 * 1024,
				QueueTTL:              60 * time.Second,
				ExpiryCleanupInterval: 2 * time.Second,
			},
			expectedError: false,
		},
		{
			name: "Invalid Configuration - Total < Model",
			config: QueueConfig{
				TotalQueueCapacity: 10 * 1024 * 1024, // 10MB
				ModelQueueCapacity: 20 * 1024 * 1024, // 20MB
			},
			expected:      QueueConfig{},
			expectedError: true,
		},
		{
			name: "Zero TotalQueueCapacity",
			config: QueueConfig{
				TotalQueueCapacity: 0,
				ModelQueueCapacity: 10 * 1024 * 1024,
			},
			expected: QueueConfig{
				TotalQueueCapacity:    DefaultTotalQueueCapacity,
				ModelQueueCapacity:    10 * 1024 * 1024,
				QueueTTL:              DefaultQueueTTL,
				ExpiryCleanupInterval: DefaultExpiryCleanupInterval,
			},
			expectedError: false,
		},
		{
			name: "Zero ModelQueueCapacity",
			config: QueueConfig{
				TotalQueueCapacity: 100 * 1024 * 1024,
				ModelQueueCapacity: 0,
			},
			expected: QueueConfig{
				TotalQueueCapacity:    100 * 1024 * 1024,
				ModelQueueCapacity:    DefaultModelQueueCapacity,
				QueueTTL:              DefaultQueueTTL,
				ExpiryCleanupInterval: DefaultExpiryCleanupInterval,
			},
			expectedError: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.validateAndApplyDefaults()
			if test.expectedError {
				if err == nil {
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if diff := cmp.Diff(test.expected, test.config); diff != "" {
					t.Errorf("validateAndApplyDefaults() mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestEvictionReason_String(t *testing.T) {
	tests := []struct {
		name           string
		evictionReason EvictionReason
		expected       string
	}{
		{
			name:           "Not Evicted",
			evictionReason: ReasonNotEvicted,
			expected:       "Not Evicted",
		},
		{
			name:           "TTL Expiry",
			evictionReason: ReasonTTLExpiry,
			expected:       "TTL Expiry",
		},
		{
			name:           "External Context Expiry",
			evictionReason: ReasonExternalContextExpiry,
			expected:       "External Context Expiry",
		},
		{
			name:           "Preempted",
			evictionReason: ReasonPreempted,
			expected:       "Preempted",
		},
		{
			name:           "Cannot Find Backend",
			evictionReason: ReasonCannotFindBackend,
			expected:       "Cannot Find Backend",
		},
		{
			name:           "Unknown Eviction Reason",
			evictionReason: EvictionReason(99), // Invalid reason
			expected:       "Unknown Eviction Reason",
		},
		{
			name:           "Negative Eviction Reason",
			evictionReason: EvictionReason(-1), // Invalid reason
			expected:       "Unknown Eviction Reason",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := test.evictionReason.String()
			if actual != test.expected {
				t.Errorf("Expected string representation '%s', but got '%s'", test.expected, actual)
			}
		})
	}
}

func TestQueueItem_checkExpiry(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name           string
		item           *queueItem
		currentTime    time.Time
		expectedError  error
		expectedReason EvictionReason
	}{
		{
			name: "TTL Expiry",
			item: &queueItem{
				request:     &mockSchedulableRequest{ctx: context.Background(), request: &types.LLMRequest{Model: "model", Criticality: v1alpha2.Critical}},
				enqueueTime: now.Add(-60 * time.Second),
				ttl:         30 * time.Second,
				done:        make(chan struct{}),
			},
			currentTime:    now,
			expectedError:  ErrEvicted,
			expectedReason: ReasonTTLExpiry,
		},
		{
			name: "Context Expiry",
			item: &queueItem{
				request: func() *mockSchedulableRequest {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					return &mockSchedulableRequest{ctx: ctx, request: &types.LLMRequest{Model: "model", Criticality: v1alpha2.Critical}}
				}(),
				enqueueTime: now.Add(-10 * time.Second),
				ttl:         30 * time.Second,
				done:        make(chan struct{}),
			},
			currentTime:    now,
			expectedError:  errors.Join(ErrEvicted, context.Canceled),
			expectedReason: ReasonExternalContextExpiry,
		},
		{
			name: "No Expiry",
			item: &queueItem{
				request:     &mockSchedulableRequest{ctx: context.Background(), request: &types.LLMRequest{Model: "model", Criticality: v1alpha2.Critical}},
				enqueueTime: now.Add(-10 * time.Second),
				ttl:         30 * time.Second,
				done:        make(chan struct{}),
			},
			currentTime:    now,
			expectedError:  nil,
			expectedReason: ReasonNotEvicted,
		},
		{
			name: "Context Expiry and TTL Expiry Reports Context Expiry",
			item: &queueItem{
				request: func() *mockSchedulableRequest {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					return &mockSchedulableRequest{ctx: ctx, request: &types.LLMRequest{Model: "model", Criticality: v1alpha2.Critical}}
				}(),
				enqueueTime: now.Add(-60 * time.Second),
				ttl:         30 * time.Second,
				done:        make(chan struct{}),
			},
			currentTime:    now,
			expectedError:  errors.Join(ErrEvicted, context.Canceled),
			expectedReason: ReasonExternalContextExpiry,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.item.checkExpiry(test.currentTime)

			if test.expectedError != nil {
				if err, ok := test.item.err.Load().(error); !ok || !cmp.Equal(err.Error(), test.expectedError.Error()) {
					t.Errorf("Expected error '%v', but got '%v'", test.expectedError, err)
				}
			} else {
				if err, ok := test.item.err.Load().(error); ok {
					t.Errorf("Expected no error, but got '%v'", err)
				}
			}

			if reason, ok := test.item.evictionReason.Load().(EvictionReason); ok && reason != test.expectedReason {
				t.Errorf("Expected eviction reason '%v', but got '%v'", test.expectedReason, reason)
			}
		})
	}
}

func TestQueueController_cleanupExpired(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name               string
		setup              func(qc *QueueController)
		expectedEvictions  int
		expectedQueueCount int
	}{
		{
			name: "No Expired Requests",
			setup: func(qc *QueueController) {
				req := &mockSchedulableRequest{ctx: context.Background(), request: &types.LLMRequest{Model: "model1", Criticality: v1alpha2.Critical}, size: 10}
				item := &queueItem{request: req, enqueueTime: now.Add(-10 * time.Second), ttl: 30 * time.Second, done: make(chan struct{})}
				qc.enqueue(item)
			},
			expectedEvictions:  0,
			expectedQueueCount: 1,
		},
		{
			name: "TTL Expiry",
			setup: func(qc *QueueController) {
				req := &mockSchedulableRequest{ctx: context.Background(), request: &types.LLMRequest{Model: "model1", Criticality: v1alpha2.Critical}, size: 10}
				item := &queueItem{request: req, enqueueTime: now.Add(-60 * time.Second), ttl: 30 * time.Second, done: make(chan struct{})}
				qc.enqueue(item)
			},
			expectedEvictions:  1,
			expectedQueueCount: 0,
		},
		{
			name: "Context Expiry",
			setup: func(qc *QueueController) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				req := &mockSchedulableRequest{ctx: ctx, request: &types.LLMRequest{Model: "model1", Criticality: v1alpha2.Critical}, size: 10}
				item := &queueItem{request: req, enqueueTime: now.Add(-10 * time.Second), ttl: 30 * time.Second, done: make(chan struct{})}
				qc.enqueue(item)
			},
			expectedEvictions:  1,
			expectedQueueCount: 0,
		},
		{
			name: "Mixed Expiry",
			setup: func(qc *QueueController) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				req1 := &mockSchedulableRequest{ctx: ctx, request: &types.LLMRequest{Model: "model1", Criticality: v1alpha2.Critical}, size: 10}
				item1 := &queueItem{request: req1, enqueueTime: now.Add(-10 * time.Second), ttl: 30 * time.Second, done: make(chan struct{})}
				qc.enqueue(item1)

				req2 := &mockSchedulableRequest{ctx: context.Background(), request: &types.LLMRequest{Model: "model1", Criticality: v1alpha2.Critical}, size: 20}
				item2 := &queueItem{request: req2, enqueueTime: now.Add(-60 * time.Second), ttl: 30 * time.Second, done: make(chan struct{})}
				qc.enqueue(item2)
			},
			expectedEvictions:  2,
			expectedQueueCount: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheduler := &mockScheduler{
				scheduleFunc: func(ctx context.Context, req *types.LLMRequest) (targetPod types.Pod, err error) {
					return &backendmetrics.FakePodMetrics{}, nil
				},
			}
			qc, err := NewQueueController(scheduler, QueueConfig{})
			if err != nil {
				t.Fatalf("Failed to create scheduler: %v", err)
			}
			qc.clock = &mockClock{currentTime: now}
			test.setup(qc)

			qc.cleanupExpired()

			queueSize := qc.QueueSize()
			if queueSize.RequestCount != uint64(test.expectedQueueCount) {
				t.Errorf("Expected queue count to be %d, but got %d", test.expectedQueueCount, queueSize.RequestCount)
			}
			evictionCount := 0
			for _, band := range qc.criticalityBands {
				for _, q := range band.queues {
					for e := q.requests.Front(); e != nil; e = e.Next() {
						item := e.Value.(*queueItem)
						if _, ok := item.err.Load().(error); ok {
							evictionCount++
						}
					}
				}
			}
			if evictionCount != test.expectedEvictions {
				t.Errorf("Expected %d evictions, but got %d", test.expectedEvictions, evictionCount)
			}
		})
	}
}
