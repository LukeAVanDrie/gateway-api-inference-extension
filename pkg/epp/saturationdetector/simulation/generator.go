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

package simulation

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// minSanityLambda enforces a floor on the arrival rate to prevent division-by-zero or effectively infinite sleep.
// 0.001 QPS = 1 request every 1000 seconds.
const minSanityLambda = 0.001

// TrafficGenerator defines the contract for a Streaming Request Source.
//
// Unlike batch generators, this interface produces requests one-by-one (Iterator Pattern), allowing for infinite
// horizon simulations with constant memory usage O(1).
//
// Thread Safety: Implementations are NOT thread-safe.
type TrafficGenerator interface {
	// Init establishes the T=0 of the generation stream.
	// It must be called exactly once before the first Peek/Generate.
	// This schedules the very first request.
	Init(now time.Time)

	// PeekNextArrival returns the absolute time of the next scheduled request without advancing the generator's state.
	PeekNextArrival() time.Time

	// GenerateNext consumes the pending arrival and returns the Request object.
	// It automatically calculates and schedules the *subsequent* arrival time.
	GenerateNext() *Request

	// SetRate updates the fundamental arrival parameter (Requests Per Second).
	// - For Constant generators: Changes Lambda immediately.
	// - For Pattern generators: updates a scalar multiplier (preserving the shape).
	SetRate(qps float64)
}

// WorkloadProfile defines the statistical shape (payload size) of the requests.
// Token counts are modeled using Log-Normal distributions to capture the "Heavy Tail" reality of LLM traffic.
type WorkloadProfile struct {
	InputMu     float64 // Mean (μ) of the underlying normal distribution for input tokens.
	InputSigma  float64 // Standard deviation (σ) of the underlying normal distribution.
	OutputMu    float64 // Mean (μ) of the underlying normal distribution for output tokens.
	OutputSigma float64 // Standard deviation (σ) of the underlying normal distribution.
}

// MeanInputTokens calculates the arithmetic mean of the input length.
// Formula: E[X] = exp(μ + σ²/2)
func (w WorkloadProfile) MeanInputTokens() float64 {
	return math.Exp(w.InputMu + (w.InputSigma*w.InputSigma)/2.0)
}

// MeanOutputTokens calculates the arithmetic mean of the output length.
func (w WorkloadProfile) MeanOutputTokens() float64 {
	return math.Exp(w.OutputMu + (w.OutputSigma*w.OutputSigma)/2.0)
}

// Standard Profiles
var (
	// ProfileBalanced: Chat/Instruction (Input ~90, Output ~148)
	ProfileBalanced = WorkloadProfile{
		InputMu: 4.5, InputSigma: 0.8,
		OutputMu: 5.0, OutputSigma: 1.0,
	}

	// ProfileHeavyContext: RAG/Summarization (Input ~3000, Output ~55)
	ProfileHeavyContext = WorkloadProfile{
		InputMu: 8.0, InputSigma: 0.5,
		OutputMu: 4.0, OutputSigma: 0.5,
	}

	// ProfileCreative: Story/Code Gen (Input ~55, Output ~1100)
	ProfileCreative = WorkloadProfile{
		InputMu: 4.0, InputSigma: 0.5,
		OutputMu: 7.0, OutputSigma: 0.8,
	}
)

// --- Base Logic (Composition) ---

// requestSampler encapsulates the RNG and Payload distribution logic.
type requestSampler struct {
	name     string
	workload WorkloadProfile
	rng      *rand.Rand
	counter  int
}

func newRequestSampler(name string, seed int64, workload WorkloadProfile) *requestSampler {
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &requestSampler{
		name:     name,
		workload: workload,
		rng:      rand.New(rand.NewSource(seed)),
	}
}

func (s *requestSampler) samplePayload() (int, int) {
	// Sample Input: X = exp(μ + σ*Z)
	inLen := int(math.Exp(s.workload.InputMu + s.workload.InputSigma*s.rng.NormFloat64()))
	inLen = max(inLen, 4) // Enforce minimum sanity (e.g., <s> + query).

	// Sample Output
	outLen := int(math.Exp(s.workload.OutputMu + s.workload.OutputSigma*s.rng.NormFloat64()))
	outLen = max(outLen, 1) // Enforce EOS existence.

	return inLen, outLen
}

// sampleInterArrival calculates the time delta for a Poisson process event.
// T = -ln(U) / λ
func (s *requestSampler) sampleInterArrival(lambda float64) time.Duration {
	// Safety clamp to prevent panic or infinite blocks
	if lambda < minSanityLambda {
		lambda = minSanityLambda
	}
	// Inverse Transform Sampling for Exponential Distribution
	seconds := s.rng.ExpFloat64() / lambda
	return time.Duration(seconds * float64(time.Second))
}

func (s *requestSampler) createRequest(arrival time.Time) *Request {
	s.counter++
	in, out := s.samplePayload()
	return &Request{
		ID:        RequestID(fmt.Sprintf("%s-%d", s.name, s.counter)),
		Arrival:   arrival,
		PromptLen: in,
		OutputLen: out,
		State:     StateQueued,
	}
}

// --- Constant Generator (Poisson Process) ---

// ConstantGenerator produces requests at a fixed average rate (Lambda).
// Memory Usage: O(1).
type ConstantGenerator struct {
	*requestSampler
	nextArrival time.Time
	qps         float64
}

var _ TrafficGenerator = &ConstantGenerator{}

func NewConstantGenerator(name string, seed int64, profile WorkloadProfile, startQPS float64) *ConstantGenerator {
	return &ConstantGenerator{
		requestSampler: newRequestSampler(name, seed, profile),
		qps:            startQPS,
	}
}

// Init seeds the first arrival based on the simulation start time.
func (g *ConstantGenerator) Init(now time.Time) {
	// The first request arrives at Start + InterArrival.
	// This prevents a burst of requests exactly at T=0.
	interval := g.sampleInterArrival(g.qps)
	g.nextArrival = now.Add(interval)
}

func (g *ConstantGenerator) PeekNextArrival() time.Time {
	return g.nextArrival
}

func (g *ConstantGenerator) GenerateNext() *Request {
	// 1. Create the request for the current slot
	req := g.createRequest(g.nextArrival)

	// 2. Schedule the next slot
	// Memoryless property: We calculate the next interval from the *current event time*, preserving the Poisson
	// distribution.
	interval := g.sampleInterArrival(g.qps)
	g.nextArrival = g.nextArrival.Add(interval)

	return req
}

func (g *ConstantGenerator) SetRate(qps float64) {
	g.qps = qps
}

// --- Time-Dependent Generators (Step, Ramp, Pulse) ---

// RateFunction defines how QPS changes over time relative to a start time.
// It returns the Base QPS.
type RateFunction func(elapsed time.Duration) float64

// ModulatedGenerator genericizes Step, Ramp, and Pulse logic.
// It calculates the instantaneous rate at the moment of the *previous* arrival to determine the delay to the *next*
// arrival.
type ModulatedGenerator struct {
	*requestSampler
	nextArrival time.Time
	startTime   time.Time
	rateFunc    RateFunction
	rateScalar  float64
}

var _ TrafficGenerator = &ModulatedGenerator{}

// Init seeds the first arrival.
func (g *ModulatedGenerator) Init(now time.Time) {
	g.startTime = now
	// Calculate initial rate
	initialRate := g.rateFunc(0) * g.rateScalar
	g.nextArrival = now.Add(g.sampleInterArrival(initialRate))
}

func (g *ModulatedGenerator) PeekNextArrival() time.Time {
	return g.nextArrival
}

func (g *ModulatedGenerator) GenerateNext() *Request {
	req := g.createRequest(g.nextArrival)

	// Calculate instantaneous rate at the current simulation time.
	// We use the arrival time of the request we just generated as the anchor for the *next* interval.
	elapsed := g.nextArrival.Sub(g.startTime)
	baseRate := g.rateFunc(elapsed)
	currentRate := baseRate * g.rateScalar

	// Schedule next.
	interval := g.sampleInterArrival(currentRate)
	g.nextArrival = g.nextArrival.Add(interval)

	return req
}

// SetRate for a ModulatedGenerator updates the Scalar Multiplier.
// If the Step function goes 10->20, and you SetRate(2.0), the generator will now go 20->40.
// This allows SetRelativeLoad to work on patterns.
func (g *ModulatedGenerator) SetRate(scalar float64) {
	if scalar <= 0 {
		scalar = 0.001 // Prevent zero-out.
	}
	g.rateScalar = scalar
}

// NewStepGenerator creates a generator that jumps from startQPS to endQPS at stepAt.
func NewStepGenerator(
	name string,
	seed int64,
	profile WorkloadProfile,
	startQPS,
	endQPS float64,
	stepAt time.Duration,
) *ModulatedGenerator {
	fn := func(elapsed time.Duration) float64 {
		if elapsed < stepAt {
			return startQPS
		}
		return endQPS
	}
	return &ModulatedGenerator{
		requestSampler: newRequestSampler(name, seed, profile),
		rateFunc:       fn,
		rateScalar:     1.0, // Default identity
	}
}

// NewRampGenerator creates a generator that linearly interpolates QPS.
func NewRampGenerator(
	name string,
	seed int64,
	profile WorkloadProfile,
	startQPS,
	endQPS float64,
	duration time.Duration,
) *ModulatedGenerator {
	slope := (endQPS - startQPS) / duration.Seconds()
	fn := func(elapsed time.Duration) float64 {
		if elapsed >= duration {
			return endQPS
		}
		t := elapsed.Seconds()
		return startQPS + (slope * t)
	}
	return &ModulatedGenerator{
		requestSampler: newRequestSampler(name, seed, profile),
		rateFunc:       fn,
		rateScalar:     1.0,
	}
}

// NewPulseGenerator creates a generator that oscillates between High and Low QPS.
func NewPulseGenerator(
	name string,
	seed int64,
	profile WorkloadProfile,
	highQPS,
	lowQPS float64,
	highDur,
	lowDur time.Duration,
) *ModulatedGenerator {
	cycleDur := highDur + lowDur
	fn := func(elapsed time.Duration) float64 {
		pos := elapsed % cycleDur
		if pos < highDur {
			return highQPS
		}
		return lowQPS
	}
	return &ModulatedGenerator{
		requestSampler: newRequestSampler(name, seed, profile),
		rateFunc:       fn,
		rateScalar:     1.0,
	}
}
