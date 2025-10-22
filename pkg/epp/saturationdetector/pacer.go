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

package saturationdetector

import (
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"
)

const DefaultPacerBurstDuration = 200 * time.Millisecond

// Pacer implements a high-precision, thread-safe Token Bucket algorithm for rate limiting.
// It smoothly enforces a target admission rate (λ).
// This implementation is generalized to support cost-aware pacing (e.g., Tokens PerSecond) in addition to QPS.
type Pacer struct {
	// clock allows for deterministic testing.
	clock clock.Clock

	// burstDuration defines the maximum duration of a burst allowed.
	// Capacity = Rate * burstDuration.
	burstDuration time.Duration

	// config stores the current *pacerConfig atomically for lock-free reads of the configuration.
	config atomic.Value // Stores *pacerConfig

	// mu protects the dynamic state of the token bucket (tokens, lastUpdate).
	mu sync.Mutex

	// tokens is the current number of available tokens/cost units. Can be fractional.
	tokens float64

	// lastUpdate is the timestamp of the last replenishment.
	lastUpdate time.Time
}

// pacerConfig holds the atomically updated configuration.
// Storing both rate and capacity ensures consistency during updates.
type pacerConfig struct {
	// rate (λ) is the target admission rate (QPS or Cost/sec).
	rate float64
	// capacity defines the maximum number of tokens (burst size).
	capacity float64
}

// NewPacer creates a new Pacer.
func NewPacer(initialRate float64, burstDuration time.Duration, c clock.Clock) *Pacer {
	initialRate = max(initialRate, 0.0)
	p := &Pacer{
		clock:         c,
		burstDuration: burstDuration,
		lastUpdate:    c.Now(),
	}

	capacity := p.calculateCapacity(initialRate)
	cfg := &pacerConfig{
		rate:     initialRate,
		capacity: capacity,
	}
	p.config.Store(cfg)

	// Initialize the bucket with a minimal burst (1 unit) to prevent large thundering herds on startup.
	// Ensure it does not exceed the calculated capacity.
	p.tokens = min(capacity, 1.0)
	return p
}

// SetRate updates the pacing rate (λ).
func (p *Pacer) SetRate(rate float64) {
	rate = max(rate, 0.0)
	newCapacity := p.calculateCapacity(rate)
	newConfig := &pacerConfig{
		rate:     rate,
		capacity: newCapacity,
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Replenish based on the *old* configuration up to the current time.
	// This is crucial for smooth transitions: we fulfill the contract of the previous rate.
	oldConfig := p.config.Load().(*pacerConfig)
	p.replenish(p.clock.Now(), oldConfig)

	// Store the new configuration atomically.
	p.config.Store(newConfig)

	// Ensure current tokens do not exceed the new capacity if the rate dropped significantly.
	p.tokens = min(p.tokens, newConfig.capacity)
}

// calculateCapacity determines the bucket capacity based on the rate.
func (p *Pacer) calculateCapacity(rate float64) float64 {
	capacity := rate * p.burstDuration.Seconds() // Capacity = Rate * BurstDuration

	// Enforce a minimum capacity of 1 unit.
	// This ensures progress at very low rates and minimizes latency introduced by waiting for fractional token
	// accumulation.
	return max(capacity, 1.0)
}

// Allow checks if a request with the given cost can be admitted and atomically consumes the tokens if so.
func (p *Pacer) Allow(cost float64) bool {
	if cost <= 0 {
		return true
	}

	cfg := p.config.Load().(*pacerConfig)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Pass the configuration that was active when the request arrived.
	p.replenish(p.clock.Now(), cfg)

	if p.tokens >= cost {
		p.tokens -= cost
		return true
	}
	return false
}

// replenish adds tokens accrued since the last update.
// Must be called under lock.
func (p *Pacer) replenish(now time.Time, cfg *pacerConfig) {
	// Calculate elapsed time.
	elapsed := now.Sub(p.lastUpdate).Seconds()

	// Always update lastUpdate to the current time.
	// This prevents deadlock if the clock skews backward (NTP adjustments, VM suspension), where 'elapsed' would be
	// negative.
	p.lastUpdate = now

	// Only accrue tokens if time has moved forward.
	if elapsed <= 0 {
		return
	}

	accruedTokens := elapsed * cfg.rate
	p.tokens = min(cfg.capacity, p.tokens+accruedTokens) // Cap the tokens at the bucket capacity.
}

// GetRate returns the current pacing rate (for observability).
// This operation is lock-free.
func (p *Pacer) GetRate() float64 {
	cfg := p.config.Load().(*pacerConfig)
	return cfg.rate
}
