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
package queuemonitor

import (
	"encoding/json"
	"sync/atomic"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

const QueueMonitorType = "QueueMonitor"

func init() {
	plugins.Register(QueueMonitorType, Factory)
}

// Monitor defines the write-side interface for the Flow Controller.
// It allows the FC to report queue depth changes in real-time.
type Monitor interface {
	// Adjust modifies the pending count by delta (positive or negative).
	Adjust(delta int64)
}

// QueueMonitorPlugin is the concrete implementation.
// It acts as the shared state bridge between FlowControl (Writer) and SaturationControl (Reader).
type QueueMonitor struct {
	typedName plugins.TypedName
	pending   atomic.Int64
}

// Factory creates the singleton monitor.
func Factory(name string, _ json.RawMessage, _ plugins.Handle) (plugins.Plugin, error) {
	return NewQueueMonitor(name), nil
}

func NewQueueMonitor(name string) *QueueMonitor {
	if name == "" {
		name = QueueMonitorType
	}
	return &QueueMonitor{typedName: plugins.TypedName{
		Type: QueueMonitorType,
		Name: name,
	}}
}

// --- SaturationController Interface (Reader) ---

// GetTotalPendingRequests returns the instantaneous count of buffered requests.
// This is O(1) and lock-free.
func (p *QueueMonitor) GetTotalPendingRequests() int {
	return int(p.pending.Load())
}

// --- FlowController Interface (Writer) ---

func (p *QueueMonitor) Adjust(delta int64) {
	p.pending.Add(delta)
}

// --- Plugin Interface ---

func (p *QueueMonitor) TypedName() plugins.TypedName {
	return plugins.TypedName{
		Type: QueueMonitorType,
		Name: QueueMonitorType,
	}
}
