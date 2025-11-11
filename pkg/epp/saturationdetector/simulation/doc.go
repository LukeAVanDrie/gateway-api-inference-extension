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

/*
Package simulation provides a high-fidelity, deterministic Discrete Event Simulation (DES) framework for validating the
dynamics of LLM Inference Flow Control systems.

It acts as a "Digital Twin" for the Saturation Controller, allowing engineers to empirically tune PID gains, verify
stability margins, and stress-test the system against complex scenarios (e.g., Cold Starts, Hardware Degradation, etc.)
without the cost or noise of a physical GPU cluster.

# Architectural Overview

The simulation orchestrates the interaction between three distinct domains:

 1. The Plant (Physics Engine):
    Modeled by [SimBackend], this simulates the non-linear behavior of a Continuous Batching inference server.
 2. The Control Plane (System Under Test):
    The real [SaturationController] is injected into the harness.
    It operates on a fixed "Slow Path" tick (e.g., 50ms), making decisions based on noisy, lagged telemetry.
 3. The Environment (Driver):
    Modeled by [SimulationEnvironment], this enforces the simulation timeline.
    It uses a Discrete Event Simulation loop to precisely interleave traffic arrivals, backend completions, and
    controller ticks.

# Fidelity Model: What IS Captured

The simulation focuses on the macroscopic dynamics that destabilize feedback loops in production:

  - The "Memory Cliff":
    It correctly models PagedAttention dynamics, including KV-cache fragmentation and the binary failure mode of OOM
    Preemption, forcing the controller to react to "Hard" memory limits (U_kv) differently than "Soft" compute limits.
  - The "Roofline" Dynamics:
    It implements a first-principles Roofline Model that accurately distinguishes between:
    1. Compute-Bound Prefill: Modeling the quadratic cost of Attention and TFLOPS constraints.
    2. Memory-Bound Decode: Modeling the linear cost of Autoregression and HBM Bandwidth constraints.
  - Observability Lag ("Dead Time"):
    By strictly simulating "Traffic Arrival" -> "Backend Processing" -> "Metric Scrape" ->"Controller Reconcile",
    the simulation accurately captures the phase lag that leads to oscillation in naive PID controllers.

# Abstractions: What is NOT Captured

To maintain deterministic execution speed, certain low-level details are abstracted:

  - Network Transport:
    Serialization, Deserialization, and TCP stack latency are assumed to be constant or negligible.
    The simulation measures "Service Latency" (Arrival at Gateway -> Completion), not "Client Latency".
  - GPU Micro-architecture:
    Complex interactions like L2 cache contention, SM occupancy, and specific kernel scheduling policies are abstracted
    into macroscopic Efficiency Scalars (MBU - Memory Bandwidth Utilization, MFU - Model Flop Utilization).

# Determinism and Reproducibility

This package is designed for strict determinism. By accepting a random seed and sorting internal event queues, it
ensures that a simulation run is bit-for-bit reproducible. This allows for binary search tuning of control parameters
(Kp, Alpha).

# Validation Methodology

The package espouses a Systems Engineering V-Model approach to testing:
 1. Define [Scenario]: A configuration of traffic patterns, hardware topology, and fault injections.
 2. Execute [Run]: The simulation produces a time-series [History] of system state.
 3. Verify [AnalyzeRun]: A statistical analyzer grades the run against [SuccessCriteria] (e.g., max overshoot,
    convergence time, tail latency), producing a detailed [ScenarioResult] scorecard.
*/
package simulation
