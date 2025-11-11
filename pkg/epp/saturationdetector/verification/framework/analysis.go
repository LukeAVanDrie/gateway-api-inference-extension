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

package framework

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/simulation"
)

const (
	// High-Density Table Formatting
	//
	// Columns:
	// 1. Time: Simulation elapsed time.
	// 2. Regime: Current FSM State.
	// 3. Mat: Maturity Counts (Immature / Maturing + Mature / Dormant).
	// 4. LI: Load Index (System Pressure).
	// 5. Out: The Actuation Output.
	//       - (C) = Concurrency Limit (Probing)
	//       - (R) = Rate Limit (Regulating)
	// 6. ^μ / μ* : Feed-Forward Capacity Estimate vs True Max Capacity (Sum).
	// 7. ^B / B* : Estimated Batch Capacity vs True Physical Capacity (Average).
	// 8. SatPV: The Process Variable (Max of Compute or Memory Pressure).
	// 9. Q_fc: Queue Depth in the Flow Control Buffer.
	// 10. E[Q]: Average Queue Depth in Backends.
	// 11. E[U]: Average Backend Utilization.
	timelineHeader = "  T+ (s) | Regime     | Mat(I/M/D) |  LI    |  u_fb  |  u_fb  | Out (Type) | ^μ / μ* (Sum) | ^B / B* (Avg) | SatPV | Q(FC) | Q(Back) Min/Avg/Max    | Util% Min/Avg/Max"
	timelineRowFmt = "  %6.2f | %-10s | %2d/%2d/%2d    | %5.2f | %6.1f | %6.1f | %6.1f (%s) | %5.1f / %-5.1f | %5.1f / %-5.1f | %5.2f | %5d | %3d / %5.1f / %-3d      | %3.0f / %3.0f / %3.0f"
)

// LogTimeline emits a formatted ASCII table of the simulation history to the test log.
// It visualizes the Controller's decision-making process alongside the Physical Ground Truth.
func LogTimeline(t *testing.T, timeline []simulation.Snapshot, maxSamples int) {
	if len(timeline) == 0 {
		t.Log("Timeline: [Empty]")
		return
	}

	startTime := timeline[0].Timestamp
	duration := timeline[len(timeline)-1].Timestamp.Sub(startTime)

	// Calculate Downsampling Step to keep logs readable
	step := 1
	if len(timeline) > maxSamples {
		step = len(timeline) / maxSamples
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n--- Simulation Timeline (Duration: %v, Data Points: %d shown / %d total) ---\n", duration, len(timeline)/step, len(timeline)))
	sb.WriteString(timelineHeader + "\n")
	sb.WriteString(strings.Repeat("-", len(timelineHeader)) + "\n")

	printRow := func(snap simulation.Snapshot) {
		elapsed := snap.Timestamp.Sub(startTime).Seconds()

		ctrl := snap.ControllerState

		// 1. Count Maturity States
		imm, mat, mature, dorm := 0, 0, 0, 0
		estBatchSum, estBatchCount := 0.0, 0
		for _, pod := range ctrl.Pods {
			switch pod.Maturity {
			case saturationdetector.Immature:
				imm++
			case saturationdetector.Maturing:
				mat++
			case saturationdetector.Mature:
				mature++
			case saturationdetector.Dormant:
				dorm++
			}

			// Only include MaturingMature pods in the ^B average to prevent "0" skew
			if pod.Maturity == saturationdetector.Maturing || pod.Maturity == saturationdetector.Mature || pod.EstimatedCapacity > 0 {
				estBatchSum += pod.EstimatedCapacity
				estBatchCount++
			}
		}

		// 2. Actuation Output
		actuationVal := 0.0
		actuationType := "-"
		switch ctrl.Regime {
		case saturationdetector.Probing:
			actuationVal = ctrl.ProbingLimit
			actuationType = "C"
		case saturationdetector.Regulating:
			actuationVal = ctrl.PacerRate
			actuationType = "R"
		}

		// 3. Ground Truth Aggregates (μ* and B*) and Backend Distribution Stats
		qMin, qMax := math.MaxInt, 0
		uMin, uMax := 1.0, 0.0
		trueMuSum, trueBatchSum := 0.0, 0.0
		podCount := 0
		for _, phys := range snap.PodPhysics {
			qMin = min(qMin, phys.QueueDepth)
			qMax = max(qMax, phys.QueueDepth)
			uMin = min(uMin, phys.Utilization)
			uMax = max(uMax, phys.Utilization)
			trueMuSum += phys.TrueServiceRate
			trueBatchSum += float64(phys.TrueBatchCapacity)
			podCount++
		}

		trueBatchAvg := 0.0
		if podCount > 0 {
			trueBatchAvg = trueBatchSum / float64(podCount)
		}

		estBatchAvg := 0.0
		if estBatchCount > 0 {
			estBatchAvg = estBatchSum / float64(estBatchCount)
		}

		fmt.Fprintf(&sb, timelineRowFmt+"\n",
			elapsed,
			ctrl.Regime.String(),
			imm, mat+mature, dorm,
			ctrl.LoadIndex,
			ctrl.FeedForwardTerm,
			ctrl.FeedBackTerm,
			actuationVal, actuationType,
			ctrl.FeedForwardTerm, trueMuSum,
			estBatchAvg, trueBatchAvg,
			ctrl.AvgSaturation,
			snap.FlowControlQueueDepth,
			qMin, snap.AverageBackendQueueDepth, qMax,
			uMin*100, snap.AverageBackendUtilization*100, uMax*100,
		)
	}

	// Loop and Downsample
	for i := 0; i < len(timeline); i += step {
		printRow(timeline[i])
	}

	// Always print the final state
	if (len(timeline)-1)%step != 0 {
		printRow(timeline[len(timeline)-1])
	}

	sb.WriteString(strings.Repeat("-", len(timelineHeader)) + "\n")
	t.Log(sb.String())
}

// Analyze processes a simulation result into a graded scorecard.
// setpoint: The target Saturation (e.g., 0.85).
// limit: The hard Saturation limit (e.g., 1.0).
// tolerance: The % error band around the setpoint (e.g., 0.05).
func Analyze(res *simulation.SimResult, setpoint, limit, tolerance float64) *Scorecard {
	s := &Scorecard{}

	// 1. Latency Analysis (From Request Log)
	s.Latency = calculateLatencyMetrics(res.CompletedRequests)

	// 2. Startup Analysis (Regime Transitions)
	s.Startup = calculateStartupMetrics(res.Timeline)
	s.Stability = calculateRegimeStability(res.Timeline)

	// 3. Control Analysis (PID Dynamics)
	// IMPORTANT: We only analyze the Longest Continuous Regulating Window.
	// Mixing Probing data with Regulating data invalidates PID metrics.
	s.Control = calculateControlMetrics(res.Timeline, setpoint, tolerance)

	// 4. Safety Analysis (Outliers)
	s.Safety = calculateSafetyMetrics(res.Timeline, res.ShedRequestCount, limit)

	// 5. Efficiency Analysis (Throughput)
	// Must use CompletedRequests for physical truth, not Controller State.
	s.Efficiency = calculateEfficiencyMetrics(res.Timeline, len(res.CompletedRequests), res.Duration)

	// 6. Accuracy Analysis (Estimators)
	s.Accuracy = calculateEstimatorMetrics(res.Timeline)

	return s
}

// --- 1. Latency Logic ---

func calculateLatencyMetrics(reqs []*simulation.Request) LatencyMetrics {
	if len(reqs) == 0 {
		return LatencyMetrics{}
	}

	var totalDispatchWait, totalService time.Duration
	latencies := make([]int64, len(reqs))
	var maxLat int64

	for i, req := range reqs {
		// End-to-End Latency
		lat := req.FinishTime.Sub(req.Arrival)
		latencies[i] = int64(lat)
		if int64(lat) > maxLat {
			maxLat = int64(lat)
		}

		// Decomposition:
		// Dispatch Wait = Time spent in Flow Control Pacer/Buffer
		// Service = Time spent in Backend (Queue + Prefill + Decode)
		totalDispatchWait += req.ScheduleTime.Sub(req.Arrival)
		totalService += req.FinishTime.Sub(req.ScheduleTime)
	}

	// Percentile Calculation
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	getPercentile := func(p float64) time.Duration {
		idx := int(math.Ceil(float64(len(latencies))*p)) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return time.Duration(latencies[idx])
	}

	count := time.Duration(len(reqs))
	return LatencyMetrics{
		P50:                getPercentile(0.50),
		P90:                getPercentile(0.90),
		P99:                getPercentile(0.99),
		Max:                time.Duration(maxLat),
		MeanDispatchWait:   totalDispatchWait / count,
		MeanBackendService: totalService / count,
	}
}

// --- 2. Startup & Stability Logic ---

func calculateStartupMetrics(timeline []simulation.Snapshot) StartupMetrics {
	if len(timeline) == 0 {
		return StartupMetrics{TimeToRegulation: -1}
	}
	start := timeline[0].Timestamp
	var timeToReg time.Duration = -1
	var peakDisc float64 = 0.0

	for _, snap := range timeline {
		// Capture Peak Discovery (L_peak)
		// In Probing, the limit is typically (L_peak + 1).
		if snap.ControllerState.Regime == saturationdetector.Probing {
			if snap.ControllerState.ProbingLimit > peakDisc {
				peakDisc = snap.ControllerState.ProbingLimit
			}
		}

		// Capture First Regulation
		if timeToReg == -1 && snap.ControllerState.Regime == saturationdetector.Regulating {
			timeToReg = snap.Timestamp.Sub(start)
		}
	}

	return StartupMetrics{
		TimeToRegulation:         timeToReg,
		PeakDiscoveryConcurrency: peakDisc,
	}
}

func calculateRegimeStability(timeline []simulation.Snapshot) RegimeStabilityMetrics {
	if len(timeline) == 0 {
		return RegimeStabilityMetrics{}
	}

	transitions := 0
	regulatingTicks := 0
	lastRegime := timeline[0].ControllerState.Regime

	for _, snap := range timeline {
		r := snap.ControllerState.Regime
		if r != lastRegime {
			transitions++
			lastRegime = r
		}
		if r == saturationdetector.Regulating {
			regulatingTicks++
		}
	}

	return RegimeStabilityMetrics{
		TransitionCount:  transitions,
		RegulatingUptime: float64(regulatingTicks) / float64(len(timeline)),
	}
}

// --- 3. Control Logic (Windowed Analysis) ---

func calculateControlMetrics(timeline []simulation.Snapshot, sp, tolerance float64) ControlMetrics {
	// 1. Segmentation: Find all continuous Regulating windows.
	// We want to analyze the PID loop only when it is continuously active.
	type window struct {
		start, end int
		duration   time.Duration
	}
	var windows []window
	currentStart := -1

	for i, snap := range timeline {
		if snap.ControllerState.Regime == saturationdetector.Regulating {
			if currentStart == -1 {
				currentStart = i
			}
		} else {
			if currentStart != -1 {
				// Window closed
				dur := timeline[i-1].Timestamp.Sub(timeline[currentStart].Timestamp)
				windows = append(windows, window{currentStart, i - 1, dur})
				currentStart = -1
			}
		}
	}
	// Handle window ending at timeline end
	if currentStart != -1 {
		dur := timeline[len(timeline)-1].Timestamp.Sub(timeline[currentStart].Timestamp)
		windows = append(windows, window{currentStart, len(timeline) - 1, dur})
	}

	// 2. Selection: Pick the longest window.
	if len(windows) == 0 {
		return ControlMetrics{} // Never regulated
	}
	best := windows[0]
	for _, w := range windows {
		if w.duration > best.duration {
			best = w
		}
	}

	// 3. Analysis: Run PID math on the stable segment.
	segment := timeline[best.start : best.end+1]
	startTime := segment[0].Timestamp

	var (
		errorSum       float64
		rateSum        float64
		rateSqSum      float64
		rateCount      float64
		peakSaturation float64
	)

	// Steady State is defined as the last 20% of this specific window.
	steadyStateStartIdx := int(float64(len(segment)) * 0.8)
	var steadySum, steadyCount float64

	for i, step := range segment {
		pv := step.ControllerState.AvgSaturation

		if pv > peakSaturation {
			peakSaturation = pv
		}

		// IAE (Integral Absolute Error)
		if i > 0 {
			dt := step.Timestamp.Sub(segment[i-1].Timestamp).Seconds()
			// Left Riemann Sum:  We assume error from previous step persisted until now.
			prevErr := math.Abs(sp - segment[i-1].ControllerState.AvgSaturation)
			errorSum += prevErr * dt
		}

		// Rate Stability
		rate := step.ControllerState.PacerRate
		if rate > 0 {
			rateSum += rate
			rateSqSum += rate * rate
			rateCount++
		}

		// Steady State
		if i >= steadyStateStartIdx {
			steadySum += (sp - pv)
			steadyCount++
		}
	}

	// Rise Time / Settling Time (Relative to start of Regulation)
	var t10, t90, lastBadTime time.Time
	var found10, found90 bool
	upper := sp * (1 + tolerance)
	lower := sp * (1 - tolerance)
	lastBadTime = startTime // Assume settled at start unless proven otherwise.

	for _, step := range segment {
		pv := step.ControllerState.AvgSaturation

		// Rise Time logic
		if !found10 && pv >= sp*0.10 {
			t10 = step.Timestamp
			found10 = true
		}
		if found10 && !found90 && pv >= sp*0.90 {
			t90 = step.Timestamp
			found90 = true
		}

		// Settling Time logic
		if pv > upper || pv < lower {
			lastBadTime = step.Timestamp
		}
	}

	var riseTime, settlingTime time.Duration
	if found10 && found90 {
		riseTime = t90.Sub(t10)
	}
	// If the last bad point is the end of the segment, we never settled.
	if lastBadTime.Equal(segment[len(segment)-1].Timestamp) {
		settlingTime = best.duration
	} else {
		settlingTime = lastBadTime.Sub(startTime)
	}

	// Overshoot
	var overshoot float64
	if peakSaturation > sp {
		overshoot = (peakSaturation - sp) / sp
	}

	// Rate Stability
	var rateStab float64
	if rateCount > 1 {
		mean := rateSum / rateCount
		variance := (rateSqSum / rateCount) - (mean * mean)
		if variance > 0 {
			rateStab = math.Sqrt(variance)
		}
	}

	// Steady Error
	var steadyErr float64
	if steadyCount > 0 {
		steadyErr = steadySum / steadyCount
	}

	return ControlMetrics{
		Duration:         best.duration,
		RiseTime:         riseTime,
		SettlingTime:     settlingTime,
		Overshoot:        overshoot,
		IAE:              errorSum,
		SteadyStateError: steadyErr,
		RateStability:    rateStab,
	}
}

// --- 4. Safety Logic ---

func calculateSafetyMetrics(timeline []simulation.Snapshot, shedCount int, limit float64) SafetyMetrics {
	maxQueue := 0.0
	satDuration := time.Duration(0)

	for i, step := range timeline {
		// Physical Queue Truth
		if step.AverageBackendQueueDepth > maxQueue {
			maxQueue = step.AverageBackendQueueDepth
		}
		// Check for hidden hotspots
		for _, phys := range step.PodPhysics {
			if float64(phys.QueueDepth) > maxQueue {
				maxQueue = float64(phys.QueueDepth)
			}
		}

		// Saturation Duration (Controller View)
		if i > 0 {
			if step.ControllerState.AvgSaturation > limit {
				dt := step.Timestamp.Sub(timeline[i-1].Timestamp)
				satDuration += dt
			}
		}
	}

	return SafetyMetrics{
		ShedCount:            shedCount,
		MaxBackendQueueDepth: maxQueue,
		SaturationDuration:   satDuration,
	}
}

// --- 5. Efficiency Logic ---

func calculateEfficiencyMetrics(timeline []simulation.Snapshot, completedCount int, duration time.Duration) EfficiencyMetrics {
	if duration == 0 {
		return EfficiencyMetrics{}
	}

	// Throughput (Physical)
	throughput := float64(completedCount) / duration.Seconds()

	// Utilization & Imbalance (Average over timeline)
	if len(timeline) == 0 {
		return EfficiencyMetrics{GlobalThroughput: throughput}
	}

	sumUtil := 0.0
	sumCV := 0.0
	cvCount := 0

	for _, step := range timeline {
		sumUtil += step.AverageBackendUtilization

		// Imbalance Calculation
		vals := make([]float64, 0, len(step.ControllerState.Pods))
		for _, p := range step.ControllerState.Pods {
			vals = append(vals, p.SaturationPV)
		}
		if len(vals) > 1 {
			mean, stdDev := stats(vals)
			// Filter idle periods to avoid CV explosion
			if mean > 0.05 {
				sumCV += (stdDev / mean)
				cvCount++
			}
		}
	}

	avgImbalance := 0.0
	if cvCount > 0 {
		avgImbalance = sumCV / float64(cvCount)
	}

	return EfficiencyMetrics{
		GlobalThroughput:   throughput,
		AverageUtilization: sumUtil / float64(len(timeline)),
		LoadImbalance:      avgImbalance,
	}
}

// --- 6. Accuracy Logic ---

func calculateEstimatorMetrics(timeline []simulation.Snapshot) EstimatorMetrics {
	sumError := 0.0
	count := 0
	// TODO: Calculate Rate Lag by cross-correlating u_ff with completed throughput
	// For now, we focus on Batch Capacity (Pipe Width).

	for _, step := range timeline {
		// Iterating over Controller State (Subjective)
		for podID, podView := range step.ControllerState.Pods {
			// Look up the Physics View (Objective Truth) for this specific pod
			physView, exists := step.PodPhysics[podID]
			// Only grade "Mature" estimates against Ground Truth
			if exists && podView.Maturity == saturationdetector.Mature {
				trueCap := float64(physView.TrueBatchCapacity)
				if trueCap > 0 && podView.EstimatedCapacity > 0 {
					err := math.Abs(podView.EstimatedCapacity-trueCap) / trueCap
					sumError += err
					count++
				}
			}
		}
	}

	avgError := 0.0
	if count > 0 {
		avgError = sumError / float64(count)
	}

	return EstimatorMetrics{
		BatchEstimatorMAPE: avgError,
		RateEstimatorLag:   0, // Placeholder
	}
}

// --- Helpers ---

func stats(values []float64) (mean, stdDev float64) {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	mean = sum / float64(len(values))

	sumSqDiff := 0.0
	for _, v := range values {
		diff := v - mean
		sumSqDiff += diff * diff
	}
	stdDev = math.Sqrt(sumSqDiff / float64(len(values)))
	return
}
