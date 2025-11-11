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
Package estimators provides high-performance, allocation-free data structures for real-time signal processing and state
estimation.

These components are foundational tools for building sophisticated monitoring and control systems, such as the
Saturation Detector used in Flow Control. They take a stream of discrete events or measurements and produce a robust,
smoothed, or resilient estimate of an underlying system property (e.g., average queue depth, peak effective batch size,
request rate).

All estimators are designed for performance-critical paths; their sample insertion methods are guaranteed to be
allocation-free. Types in this package are NOT safe for concurrent access and must be protected by external
synchronization if they are to be shared between goroutines.
*/
package estimators
