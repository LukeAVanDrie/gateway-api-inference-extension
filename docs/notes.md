# Gateway API Inference Extension - Queuing System Design Notes

## Onboarding for Future Sessions

To quickly onboard for future sessions, please review the following:

1. **`queue_design.md`:** This document provides the overall design of the queuing system, including the rationale, key components, algorithms, and future enhancements.  Pay close attention to the "Motivation and Context" section to understand the value proposition of the centralized queue.
2. **`notes.md` (this document):** This document tracks open questions, design decisions, coding tasks, and progress.  Review the current status of each question and the proposed solutions.
3. **Code Review:**  Familiarize yourself with the code for the following components:
    * `queue.go`: Implementation of the `FairRequestQueue`.
    * `queuecontroller.go`: Implementation of the `RequestQueueController`.
    * `request.go`: Request handling logic, including interaction with the queue and scheduler.
    * `requestsizer.go`:  Request size estimation logic.
    * `scheduler.go` and `filter.go`: (Optional)  Review the scheduler code for context, but the focus should be on the queueing system.
4. **Key Concepts:**  Ensure you understand the following key concepts:
    * Weighted Fair Queuing (WFQ)
    * Stock and Flow Model with Hysteresis Band
    * Preemption Algorithm
    * Dynamic Request Sizing (transitioning from word count to tokens)
    * Popper Goroutine Behavior


## Question 1: Request Size Integration with WFQ and Stock

**Priority:** High
**Estimated Time:** 4-8 hours

**Current Status:** Implemented. Request size is integrated using `req.Size()` in `FairRequestQueue.Push`, `RequestQueueController.OnRequestScheduled/OnRequestComplete`, and `RequestQueueController.TryEnqueue`. Benchmarks and further performance analysis (1.11) are pending.  The current implementation relies on the "prompt" field, which is a significant limitation (Question 30).  *Word count is a poor proxy for actual resource usage.*

**Proposed Solution/Plan:**  Transition to token-based size estimation is crucial (Question 30). Investigate performance implications of dynamic request sizing and handling streaming request bodies.

* **Request Size Unit:** Currently based on word count.  Should transition to tokens.
* **Size Estimation Method:** Currently uses `WordCountSizer`.  Needs to be replaced with a token-based sizer.
* **WFQ Integration:** `virFinishTime` calculation uses `req.Size()`, which is currently based on word count.
* **Stock Integration:** Stock is the sum of `req.Size()` for in-flight requests.

**Open Questions:**

* Performance implications of dynamic request sizing (benchmarks needed - 1.11).
* How to handle streaming request bodies for size estimation.
* Which tokenization library/method to use for future automatic size estimation?
* *What are the expected performance characteristics of the `WordCountSizer`? How much error does it introduce compared to token-based sizing?*

**Coding Tasks:**

* 1.6: Add unit tests for `FairRequestQueue` with varying request sizes. **TODO**
* 1.10: Add unit tests for `RequestQueueController` with varying request sizes. **TODO**
* 1.11: Benchmark performance with dynamic request sizes.  *Crucial for validating performance impact.*
* 1.12: Update unit tests in server_test.go to cover size calculation **TODO**  *(Existing tests might need updating to reflect the current `req.Size()` implementation and the use of `WordCountSizer`.)*

**Documentation Updates:**

* Update `queue_design.md` to reflect that `req.Size()` is integrated but based on the suboptimal `WordCountSizer`.

## Question 2: Popper Goroutine Behavior

**Priority:** High (Increased due to locking changes)
**Estimated Time:** 4-6 hours (Increased due to potential refactoring)

**Current Status:** Multiple poppers with constant jitter. Pushes and pops use separate helper functions with optimized locking.  *`Pop` acquires a write lock (`frq.mu.Lock()`).* Dynamic adjustment of poppers is not yet implemented. The rationale for multiple poppers needs revisiting given the optimized locking, especially since `Pop` now acquires a write lock and not a read lock as previously stated.  Current implementation and design docs assume multiple poppers, requiring updates.

**Proposed Solution/Plan:** Investigate single popper with backoff *or* dynamic adjustment based on queue length and/or backend load. Benchmark both against current implementation.  Explore alternative concurrency control mechanisms (e.g., channels, semaphores) and their performance implications.  Consider random start times for poppers to avoid tight-loop behavior on startup.

**Open Questions:**

* What are precise performance characteristics (throughput, latency, CPU usage) of single vs. multiple poppers under various load conditions? Should we change the design doc and remove the multiple poppers with constant jitter assumption?
*  Which dynamic adjustment strategy is most suitable (queue length-based, backend load-based, or a combination)?
* Do alternative concurrency control mechanisms offer significant advantages over the current approach?

**Coding Tasks:**

* Implement single popper with backoff.
* Implement dynamic popper adjustment (at least one strategy).
* Benchmark all popper strategies under varying load conditions (low, medium, high, burst). Collect metrics for throughput, latency, and CPU usage.
* Update or remove existing unit tests for `FairRequestQueue` and `RequestQueueController` related to popper behavior.

**Documentation Updates:**

*  Thoroughly revise popper behavior documentation in `queue_design.md` to reflect the chosen strategy (single, multiple, or dynamic) and its rationale. Remove outdated "increased scheduling probability" argument. Clearly explain the concurrency control mechanism, number of poppers (if fixed), and performance trade-offs.

## Question 3: Flow Isolation and Backend Congestion

**Priority:** Low
**Estimated Time:** Deferred

**Current Status:** No mechanism for flow isolation at the backend level.

**Proposed Solution/Plan:**  Defer this to future work.  Document it as a known limitation.

**Open Questions:**

*  What are the potential approaches for addressing this in the future?  (Brainstorming ideas welcomed.)

**Coding Tasks:** None for now.

**Documentation Updates:**

* Explicitly state the limitation of flow isolation in the documentation. Briefly mention potential future directions (e.g., flow-specific concurrency limits at the backend).


## Question 4: Scheduler Interaction and Backpressure

**Priority:** Low
**Estimated Time:** Deferred

**Current Status:** Queue controller and scheduler are decoupled. No direct backpressure mechanism from scheduler to controller.

**Proposed Solution/Plan:** Maintain the decoupled design for now.  Consider future enhancements based on aggregate metrics from the scheduler.

**Open Questions:**

* What specific aggregate metrics from the scheduler could be useful for the queue controller?
* How would these metrics be integrated into the stock calculation or controller parameters?

**Coding Tasks:** None for now.

**Documentation Updates:**

*  Clarify the decoupling and its rationale (simplicity, modularity).
*  Mention potential future enhancements using aggregate scheduler metrics.


## Question 5: Error Handling Fail-Open Strategy for Dequeue

**Priority:** Low
**Estimated Time:**  1 hour

**Current Status:** Fail-open strategy for dequeue errors. Currently, `ErrQueueEmpty` is the only dequeue error.

**Proposed Solution/Plan:** Keep the current fail-open strategy.  Re-evaluate if other dequeue error types are introduced.

**Open Questions:** None for now.

**Coding Tasks:** None.

**Documentation Updates:** No changes needed.


## Question 6: Priority-Based Preemption Details

**Priority:** High
**Estimated Time:** 4-8 hours

**Current Status:**  Preemption design partially implemented in `queue.go` but incomplete and currently not compiling.  *Requires careful review and thorough testing once completed.*

**Proposed Solution/Plan:**

* **Trigger:** Preemption is triggered only when the *total* queue is full (across all flows) and a higher-priority request arrives.  Individual flow queues do not trigger preemption.
* **Re-queueing:** Preempted requests are *not* re-queued. Their context is canceled (fail-close).
* **Victim Selection:** Use the maximum `virFinishTime` (inverse WFQ) within the lowest priority flow.

**Open Questions:**

* *What are the specific error handling strategies within `preemptRequest`? How are edge cases handled, such as preempting from an empty queue or a queue with only one element?*

**Coding Tasks:**

* 6.1: Implement preemption logic in `FairRequestQueue.Push`... (In progress - incomplete)
* 6.2: Implement context cancellation for preempted requests.  (Pending)
* 6.3: Add unit tests for preemption logic.  *Essential for verifying correctness.*  *Include edge cases and boundary conditions.*

**Documentation Updates:**

* Update `queue_design.md` with details of the implemented preemption algorithm and error handling.

## Question 7: Metrics Granularity

**Priority:** Medium
**Estimated Time:** 1-2 hours

**Current Status:** Metrics are labeled with both `model` and `targetModel`.

**Proposed Solution/Plan:**  No changes needed to the metrics labeling.  However, consider whether the queuing system itself should use `targetModel` in its logic (currently only uses `model`).

**Open Questions:**

* Should the queuing system differentiate between `model` and `targetModel` in its logic (e.g., separate queues or stock calculations for different target models)?  What are the trade-offs?

**Coding Tasks:**  None for now, unless we decide to incorporate `targetModel` into the queueing logic.

**Documentation Updates:**  Clarify that the queuing system currently uses only `model` for its logic, but metrics are labeled with both `model` and `targetModel`.


## Question 8: Stock Management with Dynamic Request Sizes

**Priority:** High
**Estimated Time:** 2-4 hours

**Current Status:** Implementation complete, pending unit tests.

**Proposed Solution/Plan:**

* **Stock Calculation with Size():** Modify `OnRequestScheduled` and `OnRequestComplete` in `RequestQueueController` to use `req.Size()` instead of incrementing/decrementing by 1.

* **Hysteresis Band Calculation:** Ensure the `lowerBound` and `upperBound` calculations in `RequestQueueController` are correct when dealing with potentially large request sizes. Consider adjustments or scaling factors if necessary.

* **TryEnqueue Logic with Dynamic Sizes:** Modify the `TryEnqueue` logic to consider the size of the incoming request and only bypass if there's enough "headroom" within the `upperBound`.

* **Benchmarking with Dynamic Sizes:** Update benchmarking scenarios (Question 19) to include varying request sizes.

**Open Questions:**  None.

**Coding Tasks:**
* 8.4: Add unit tests for stock management with dynamic request sizes. **TODO**

**Documentation Updates:**

*  Update the design doc to reflect these changes.

## Question 9: Popper Behavior with Empty Priority Queues

**Priority:** Medium
**Estimated Time:** 1-2 hours

**Current Status:** Unclear how poppers behave when higher-priority queues are empty.

**Proposed Solution/Plan:** Poppers will automatically dequeue from the next highest priority queue.  `FairRequestQueue.Pop()` will handle the selection logic.

**Open Questions:** None.

**Coding Tasks:**

* Implement the cascading priority selection logic in `FairRequestQueue.Pop()`.

**Documentation Updates:**

* Update `queue_design.md` to describe the popper behavior with empty priority queues.

## Question 11: Interaction with Scheduler during Backend Unavailability

**Priority:** Medium
**Estimated Time:** 1-2 hours

**Current Status:**  Unclear how the queuing system interacts with the scheduler during backend unavailability.

**Proposed Solution/Plan:**  The controller tracks in-flight requests ("stock") and aims to maintain a watermark per backend, scaled by the number of ready backends.  When no backends are ready, the watermark becomes zero, automatically stopping dequeueing.

**Open Questions:** None.

**Coding Tasks:**

* Ensure the stock calculation and watermark adjustment logic in `RequestQueueController` correctly handles zero ready backends.

**Documentation Updates:**

*  Describe the interaction with the scheduler during backend unavailability in `queue_design.md`.


## Question 12: Metrics for Preemption

**Priority:** High
**Estimated Time:** 2-4 hours

**Current Status:** Metrics defined. Ready for implementation. Should we actually remove `preemption_time_ms` since we are not requeueing preempted requests?  Consider adding new metrics related to the *reason* for preemption (e.g., capacity limit, priority-based preemption). This would provide more fine-grained insights into preemption behavior.

**Proposed Metrics:**

* `preemption_count{model, priority}`:  Number of preempted requests per model and priority level.
* `requests_preempted_total{criticality_level, victim_criticality_level}`: Total number of requests preempted, categorized by incoming and preempted request criticality.
* `preemptions_total{reason}`:  Total number of preemptions, labeled by the reason (capacity limit, priority-based preemption, etc.).

**Coding Tasks:**
* 12.1: Implement the defined preemption metrics.
* 12.2: Add unit tests for metric collection.


**Documentation Updates:**

* Update the list of preemption metrics in `queue_design.md`, including the new `preemptions_total{reason}` metric. Clearly explain the meaning and usage of each metric.

## Question 13: Long-Term Vision for Scheduler Integration

**Priority:** Low
**Estimated Time:** Deferred

**Current Status:** No long-term plan for tighter scheduler integration.

**Proposed Solution/Plan:**  No concrete plans yet.  Decoupled design allows for future integration as needed.

**Open Questions:**  What are the potential benefits and challenges of tighter integration?

**Coding Tasks:** None for now.

**Documentation Updates:**

* Briefly mention the current decoupled approach and the potential for future tighter integration in `queue_design.md`.


## Question 14: Preemption and WFQ Interaction

**Priority:** High
**Estimated Time:** 1-2 hours

**Current Status:** Preemption logic implemented, but interaction with `virtualTime` needs careful analysis and potential adjustment.

**Proposed Solution/Plan:**  Determine the correct approach for managing `virtualTime` during preemption:
* **Option 1 (No Adjustment):**  Simplest approach, but could lead to unfairness if preempted requests have already contributed to `virtualTime`.  Needs careful analysis and benchmarking.
* **Option 2 (Adjust `virtualTime`):** Subtract preempted request's size from `virtualTime`.  More complex, but potentially fairer.  Needs precise implementation and thorough testing.
* **Option 3 (Reset `virtualTime` for Flow):**  If all requests for a flow are removed due to preemption or expiry, reset `virtualTime` to `f.lastVirFinishTime` for that flow. This ensures that expired flows don't unduly influence future `virtualFinishTime` calculations for that particular flow.


**Open Questions:**
* Which `virtualTime` adjustment strategy is most fair and efficient?
* How should `virtualTime` be handled in edge cases (e.g., preempting the last request in a flow)?
* *What are the performance implications of each option, especially under high load and with varying request sizes?*

**Coding Tasks:**
* Implement each `virtualTime` adjustment option.
* Benchmark each option with varying load conditions and request sizes, focusing on fairness and throughput.
* Add unit tests to verify the correctness of each option and handle edge cases.
* Add logging to track `virtualTime` changes during preemption and expiry for debugging and analysis.

**Documentation Updates:**

*  Clearly document the chosen `virtualTime` adjustment strategy and its rationale in `queue_design.md`, including explanation of the trade-offs and performance implications.

## Question 16: Critical Evaluation of Design and Mitigation Strategies

**Priority:** High
**Estimated Time:** Ongoing/Incorporated into other tasks

**Current Status:**  Concerns about the effectiveness of the current design in achieving fairness, prioritization, performance, and resource utilization objectives due to the autoregressive nature of LLMs and simplifications in the current implementation.

**Discussion Points and Potential Mitigation Strategies:**

* **Fairness (WFQ):**
    * **Challenge:** Current fixed `requestSize` can lead to unfairness if request complexity varies. Backend congestion (due to varying compute times and backend queuing/batching) can further exacerbate unfairness.
    * **Mitigation:** Implement dynamic request sizing (token counts initially, explore more sophisticated methods later).  Monitor preemption rates to ensure preemption doesn't unduly disrupt WFQ fairness.  Consider flow isolation at the backend level in future work.

* **Prioritization (Preemption):**
    * **Challenge:**  Current cascading priority scheme (critical > default > sheddable) can lead to starvation.  Flow priority is currently based on criticality, which might not be the most flexible approach.
    * **Mitigation:** Allow users to configure priority based on performance objectives (e.g., latency SLAs) or weighting.  Explore alternative prioritization schemes that balance preemption with fairness.  Consider more sophisticated control loops (PI, PID) for managing stock to reduce preemption frequency. *Explore preemption limits (e.g., maximum number or size of preempted requests per incoming request) to prevent excessive preemption.*


* **Improved Performance (Stock Management):**
    * **Challenge:** Current stock calculation (simple request count) is not sensitive to request complexity, impacting the accuracy of the backpressure mechanism. Scheduler's backend selection might not align perfectly with queue controller's stock-based decisions.
    * **Mitigation:** Integrate dynamic request sizing into stock calculation.  Investigate tighter integration with the scheduler to improve responsiveness to backend congestion. Explore PI/PID control loops for stock management. *Consider request size classes and flow-specific stock management to address potential issues with heterogeneous request sizes.*


* **Efficient Resource Utilization:**
    * **Challenge:**  Autoregressive nature of LLMs makes accurate resource estimation difficult. Decoupling from the scheduler could lead to suboptimal resource allocation.
    * **Mitigation:**  Refine stock management with dynamic request sizing and potential integration with scheduler metrics.  Explore alternative queue management strategies that take backend load into account.

* **Other Considerations:**
    * **Preemption Tuning and Monitoring:** Implement comprehensive metrics to monitor preemption rates and potential starvation.  Tune preemption parameters carefully to balance responsiveness and stability.
    * **Lack of Flow Isolation at Backend:** Document this limitation clearly. Explore strategies for flow isolation at the backend level in future work.
    * **Backend Agnosticism (Strength and Weakness):** While backend agnosticism simplifies queue management and allows for flexibility in backend selection, it limits the queue's ability to respond to flow-specific congestion at the backend level. This trade-off should be carefully considered.


**Coding Tasks:**

* Implement the identified mitigation strategies, starting with dynamic request sizing.

**Documentation Updates:**

*  Update `queue_design.md` to reflect the discussion points and chosen mitigation strategies.


## Question 17: Motivation and Context for Centralized Queuing

**Priority:** High
**Estimated Time:** 2 hours

**Current Status:** Design doc lacks a strong introduction explaining the specific challenges addressed by the queuing system and its value proposition.

**Goal:**  Clearly articulate the motivation for centralized queuing in the LLM serving context, emphasizing its benefits *despite* the presence of individual backend queues and a scheduler that is aware of backend load and variant affinity.

**Points to Emphasize:**

* **Unpredictable Nature of LLM Inference:**  LLM inference requests have highly variable processing times due to their autoregressive nature. This makes it difficult for individual backend queues and even a sophisticated scheduler to perfectly predict and manage resource allocation.  A centralized queue can act as a buffer, smoothing out these variations and improving overall system stability.
* **Fairness Across Flows:**  While the scheduler can consider backend load and variant affinity, it might not explicitly prioritize fairness across different models or flows.  A centralized queue with WFQ can guarantee fair access to backends, preventing starvation for specific models or flows, even if they have unpredictable or highly variable request processing times.  This is especially critical for multi-tenant scenarios.
* **Decoupling and Modularity:**  Decoupling the queuing logic from the scheduler simplifies both components and allows for independent evolution and optimization.  The scheduler can focus on backend selection and variant affinity, while the queue can focus on fairness, prioritization, and backpressure management. This modularity also facilitates experimentation with different queuing algorithms and control mechanisms without modifying the scheduler.
* **Global Optimization vs. Local Optimization:**  Individual backend queues perform local optimization, managing requests for their specific backend. The scheduler also operates at a per-backend level, considering individual backend loads and affinities.  A centralized queue enables global optimization, considering the overall system load and fairness across all flows. This global perspective can lead to better resource utilization and performance than purely local optimization.  For example, the centralized queue can apply backpressure globally, preventing any backend from becoming overloaded, even if the scheduler's local decisions might lead to uneven load distribution. This is particularly important for inference extension backends, where even a single overwhelmed backend can significantly impact overall performance.  Furthermore, the scheduler can make suboptimal decision when there is contention for an available backend.
* **Enhanced Control Mechanisms:** A centralized queue allows for implementing more sophisticated control mechanisms, such as dynamic stock management, hysteresis bands, and preemption, which are difficult or impossible to implement with individual backend queues.  These control mechanisms can improve stability, responsiveness, and resource utilization under varying load conditions.  Specifically, the controller enables a global hysteresis band strategy, smoothing out the backend resource allocation behavior. This helps prevent rapid oscillations between queueing and bypassing and improves overall system stability under fluctuating load conditions.  Moreover, a centralized queue controller offers a global view of the resource allocation, enabling more effective backpressure mechanisms and preventing cascading failures due to localized backend congestion. This is critical for mitigating the impact of the unpredictable processing times characteristic of LLM inference.
* **Simplified Integration with External Systems:** A centralized queue provides a single point of integration for external monitoring, management, and control systems. This simplifies integration and enables more sophisticated management capabilities, such as dynamic scaling of backend resources based on queue length or implementing external priority management systems.

**Coding Tasks:**  None (documentation update only)

**Documentation Updates:**

* Add a "Motivation and Context" section to the design doc (`queue_design.md`) incorporating the points listed above. Explain the value proposition clearly and concisely, anticipating potential pushback from reviewers familiar with existing backend queues and scheduler functionality.

## Question 18: Document Goroutine Management

**Priority:** High
**Estimated Time:** 1 hour

**Current Status:**  The design document lacks a clear description of the goroutines involved in the queuing system and their interactions.

**Goal:**  Provide a concise overview of the goroutines involved, their responsibilities, and how they interact to manage request flow and concurrency.

**Points to Include:**

* **Request Goroutine:**  Each incoming request is handled by its own dedicated goroutine. This goroutine is responsible for the entire request lifecycle, from entry to completion.  It interacts with the `RequestQueueController` for queueing/bypass decisions and with the scheduler for backend selection.

* **Queue Controller Goroutine:** The `RequestQueueController` runs in a single, dedicated goroutine.  It manages the overall queueing and dequeueing logic, monitors the stock, and makes enqueue/bypass/reject decisions. It also launches and manages the popper goroutines and the expiry cleanup goroutine.

* **Popper Goroutines:** Multiple popper goroutines are launched by the `RequestQueueController`. These goroutines continuously attempt to dequeue requests from the `FairRequestQueue` when the stock is below the lower bound. They interact with the queue using its `Pop()` method and with the controller by incrementing the stock when a request is dequeued and scheduled (`OnRequestScheduled`).

* **Expiration Cleanup Goroutine:** A dedicated goroutine periodically cleans up expired requests from the `FairRequestQueue` using the `CleanupExpired()` method. This goroutine is managed by the `RequestQueueController`.

* **Scheduler Interaction (within Request Goroutine):**  The scheduler logic is executed *within* the request goroutine after the queueing/bypass decision is made by the `RequestQueueController`.  This interaction should be clearly explained to avoid confusion.

* **Concurrency Control:** Briefly mention the use of synchronization primitives (mutexes, RWMutexes) to protect shared data structures (queue, stock) from race conditions.

**Coding Tasks:** None (documentation update only)

**Documentation Updates:**

* Add a section to `queue_design.md` describing the goroutine management and concurrency model as outlined above.  This section should ideally include a diagram illustrating the goroutine interactions.

## Question 19: Benchmarking Scenarios

**Priority:** High
**Estimated Time:** 2-4 hours

**Current Status:**  No specific benchmarking scenarios defined to validate the queuing system's effectiveness.

**Goal:** Define a set of benchmarking scenarios that cover various load conditions, request sizes, and flow priorities to thoroughly evaluate the queuing system's performance, fairness, and resource utilization.

**Proposed Scenarios:**

* **Varying Load:**
    * Low Load:  Request arrival rate significantly below backend capacity.  Verify bypass behavior and minimal queueing.
    * Moderate Load: Request arrival rate near backend capacity. Verify efficient queueing and dequeueing.
    * High Load: Request arrival rate exceeds backend capacity. Verify backpressure, fairness, and preemption (once implemented).  Measure rejection rates for different priority levels.
    * Burst Load:  Sudden spikes in request arrival rate.  Verify responsiveness of stock management and stability under transient load conditions.

* **Varying Request Sizes:**  (Once dynamic request sizing is implemented)
    * Small Requests: Simulate requests with small sizes (e.g., short prompts).
    * Large Requests: Simulate requests with large sizes (e.g., long documents).
    * Mixed Requests:  A combination of small and large requests.  Verify fairness under varying request size conditions.

* **Varying Flow Priorities:**
    * Single Flow:  Benchmark with a single flow to establish baseline performance.
    * Multiple Flows with Equal Priority:  Verify fairness and resource allocation across flows with equal priority.
    * Multiple Flows with Different Priorities:  Verify prioritization and preemption behavior (once implemented).  Measure impact on latency and throughput for different priority levels.

* **Backend Unavailability:**
    * Temporary Backend Unavailability:  Simulate temporary backend outages or scaling events. Verify queue controller's response and recovery.

**Additional Scenarios:**

* Mixed Request Sizes and Priorities:  Combine varying request sizes and priority levels to simulate realistic workload conditions. Verify fairness and prioritization behavior. Include uniform and pareto size distributions for requests.
* High Load with Preemption:  Simulate high load conditions with multiple flows and priorities.  Measure preemption rates, latency for different priority levels, and overall throughput.


**Metrics to Collect:**

* Throughput (requests per second)
* Latency (p99, p95, p50)
* Queue Length (average, maximum)
* Stock Level (average, maximum)
* Rejection Rate (per flow, per priority level)
* Preemption Rate (per flow, per priority level - once preemption is implemented)
* Backend Utilization (CPU, memory)


**Coding Tasks:**

* Implement the benchmarking framework and the defined scenarios.

**Documentation Updates:**

*  Add a section to `queue_design.md` describing the benchmarking methodology and the specific scenarios used for evaluation.

[...] (Previous content unchanged)

## Question 20: Heterogeneous Backend Capacities

**Priority:** Medium  (Deferred - will be addressed after core queuing logic is finalized)
**Estimated Time:** 4-6 hours

**Current Status:** Deferred.  Will be addressed after the homogeneous backend case is implemented and tested.

**Proposed Solution/Plan:**

1. **Backend Capacity Metric:** Introduce a capacity metric for each backend. This metric could represent the number of requests a backend can handle concurrently, the amount of available memory, or a combination of factors.  We'll need to define a suitable metric and a way to obtain it (e.g., from the scheduler or through a separate monitoring system).

2. **Weighted Stock Calculation:**  Modify the stock calculation in `RequestQueueController` to weight each request's size by the capacity of the backend it's assigned to.  For example, if a backend has twice the capacity of another, a request scheduled to that backend would contribute twice as much to the stock. This approach will allow the controller to account for varying backend capacities and make more informed queuing/bypass decisions.

3. **Weighted Hysteresis Bounds:** The hysteresis bounds (`lowerBound`, `upperBound`) should also be weighted by the average backend capacity.  This will ensure that the bounds adapt to changes in the overall backend capacity.  We might need to experiment with different weighting schemes to find the optimal balance.

4. **Scheduler Integration:**  We'll need to integrate with the scheduler to obtain the backend capacity metric. This might involve adding a new interface or extending the existing `PodMetricsProvider`.  We'll need to carefully consider the design implications of this integration.

5. **Benchmarking:** We'll need to update the benchmarking scenarios to include heterogeneous backend capacities.  This will allow us to validate the effectiveness of the weighted stock calculation and hysteresis bounds under realistic conditions.


**Open Questions:**

* How to define and obtain the backend capacity metric?
* What's the most appropriate weighting scheme for the stock calculation and hysteresis bounds?
* How to best integrate with the scheduler to obtain backend capacity information?


**Coding Tasks:**

* Implement the weighted stock calculation and hysteresis bounds in `RequestQueueController`.
* Implement the necessary scheduler integration to obtain backend capacity information.
* Update the benchmarking scenarios to include heterogeneous backend capacities.
* Update unit tests.

**Documentation Updates:**

* Update `queue_design.md` to describe the handling of heterogeneous backend capacities.

## Question 22: Request Size Determination and Interface Segregation

**Priority:** High
**Estimated Time:** 2-4 hours

**Current Status:** Resolved.

**Decisions and Plan:**

* **Request Size Determination:** Calculate request size within the Inference Extension using `WordCountSizer`.  Store size in `RequestContext.QueueSize`.

* **Interface Segregation:** Refactor `scheduling.QueueableRequestContext` into separate `RequestProperties` and `RequestContext` interfaces.

**Coding Tasks:**

* Add unit tests.

**Documentation Updates:**

*  Update `queue_design.md` to reflect the chosen approach for request size determination and the refactored interfaces.

## Question 23: General Code Improvements (Non-Queuing Related)

**Priority:** Medium
**Estimated Time:** Variable

**Current Status:** Additional suggestions added.

**Suggestions:**

* **Error Handling in `Process` (server.go):** Improve error handling, especially around `s.datastore.FetchModelData`.
* **Metrics (server.go):** Add more detailed metrics.
* **Unnecessary Marshal/Unmarshal in `HandleRequestBody` (request.go):** Optimize request body handling to avoid unnecessary marshaling/unmarshaling.
* **`IsCritical` Calculation (request.go):** Verify that basing criticality on the original model (not `targetModel`) is the desired behavior. (Confirmed as intended behavior, so no action required).


**Coding Tasks:** Implement the suggested improvements.

**Documentation Updates:**  None.

## Question 24: Request Size Interface

**Priority:** Resolved
**Estimated Time:** N/A

**Current Status:** Resolved. The `Size()` method will be added to the *new* `RequestProperties` interface.

## Question 25: Handling Streaming Request Bodies

**Priority:** Low (Deferred - assumption made)
**Estimated Time:** N/A

**Current Status:** Deferred.  Assuming request body is received in a single chunk.

**Assumption:** Request bodies are received in a single chunk, allowing for straightforward size determination.  This assumption simplifies the initial implementation.  Revisit this question if the assumption proves invalid.

**Documentation Updates:**

* Document the assumption about single-chunk request bodies in `queue_design.md`.

## Question 26: Tokenization Library Selection


**Priority:** Medium
**Estimated Time:** 1-2 hours

**Current Status:** RequestSizer interface and WordCountSizer implemented.  Integration into HandleRequestBody complete.

**Decision:** Define an interface for request size estimation and provide an initial concrete implementation using a word count heuristic.

**Coding Tasks:**
* 26.4: Add unit tests.

## Question 27: Queue/Scheduler Prioritization Interaction

**Priority:** High
**Estimated Time:** 2 hours (design and discussion)

**Current Status:**  Analysis and recommendations added.  Ready for discussion and documentation.

**Analysis:**

The queue prioritizes requests based on model criticality and WFQ, providing fairness and preventing starvation *at the queue level*.  The scheduler prioritizes *backends* based on factors like queue length, KV cache usage, and LoRA affinity, aiming for efficient resource utilization.

The key interaction point is the bypass mechanism:

* **Low Stock:**  The queue bypasses requests, allowing the scheduler to make decisions based on backend-specific criteria.
* **High Stock:**  The queue controls which requests reach the scheduler, prioritizing critical requests and using WFQ to maintain fairness among queued requests.

**Potential Issues:**

* A bypassed request might be deemed "sheddable" by the scheduler if backend load is high.  This could lead to unintended rejection of bypassed requests.

**Recommendations:**

* **Document Behavior:**  Clearly document the bypass behavior and the interaction between queue and scheduler prioritization in `queue_design.md`.
* **Scheduler Feedback (Future Enhancement):**  For tighter integration, the scheduler could provide feedback to the queue controller about backend load. This would allow the queue to make more informed bypass decisions.
* **Pass Request Size to Scheduler:** The scheduler's backend selection currently doesn't consider request size.  Passing this information to the scheduler would allow it to make more informed decisions, especially under high load or with heterogeneous backends. This could help mitigate the potential issue of bypassed requests being deemed sheddable and rejected.

**Open Questions:**

* How should the system handle the case where a bypassed request is deemed "sheddable" by the scheduler?

**Coding Tasks:** None (design and discussion).

**Documentation Updates:**

* Update `queue_design.md` to describe the interaction and recommendations.  Include a discussion of the "bypassed but sheddable" scenario.

## Question 28: Handling Missing or Invalid "prompt" Field

**Priority:** Resolved
**Estimated Time:** N/A

**Current Status:** Resolved. Fail close by returning an error.

**Decision:** Return an error (gRPC status `InvalidArgument`) if the "prompt" field is missing or not a string in the request body.  This prevents the system from processing invalid requests.

**Coding Tasks:**
- Add a unit test for the error condition

**Documentation Updates:**

* Document the error handling for missing/invalid "prompt" in `queue_design.md`.


## Question 29: Error Handling, Metrics, and Cleanup

**Priority:** High
**Estimated Time:** 4-6 hours

**Current Status:**  Initial review complete.  Issues identified.

**Proposed Solution/Plan:**
* Improve error handling by returning more specific error codes based on the underlying cause.  (e.g., in `server.go:Process`)
* Handle `s.datastore.FetchModelData` failures gracefully.  (in `server.go:Process`)
* Add metrics related to queue length, wait times, and bypass rates.
* Ensure `OnRequestComplete()` is *always* called, even if `HandleRequestBody` returns an error, to avoid stock calculation inaccuracies.

**Open Questions:**  None.

**Coding Tasks:**
* 29.1 Implement detailed error handling throughout request processing.
* 29.2 Implement queue metrics (length, wait time, bypass rate).
* 29.3 Ensure proper `OnRequestComplete` cleanup throughout request lifecycle, especially in `HandleRequestBody`.
* 29.4: Implement handling and testing for zero ready backends.  Consider context cancellation for requests stuck in the queue.
**Documentation Updates:**

**Documentation Updates:**
* Document improved error handling and new metrics in queue_design.md.
* Add documentation on handling zero ready backends.



## Question 30: Request Sizing Flexibility

**Priority:** Medium
**Estimated Time:** 4-6 hours

**Current Status:**  Sizing currently relies on word count of the "prompt" field. This is a limiting assumption.

**Proposed Solution/Plan:**
* Allow configuration of which field to use for size calculation.
* Support calculating size based on the entire request body if needed.

**Open Questions:**
* Best way to handle configuration (environment variables, config file, etc.)?
* How to dynamically determine the relevant field for different models or APIs?

**Coding Tasks:**
* 30.1: Implement configuration options for request sizing.
* 30.2: Implement size calculation based on the configured field or entire body.
* 30.3: Update tests to cover various sizing scenarios.

**General Notes about RequestQueueController (add to appropriate sections):**

* **Zero Backends Handling:** What happens if the number of ready backends becomes zero *after* a request is enqueued?  (Potential for requests to get stuck. Consider context cancellation.)
* **Popper Fairness:**  How is fairness guaranteed across priority levels with multiple poppers?  (If one popper is faster, could it lead to unfairness?)


**General Notes about FairRequestQueue (add to appropriate sections):**

* **`CleanupExpired` Race Condition:** Potential race condition in `CleanupExpired` due to concurrent access.  (Consider finer-grained locking.)
* **`virtualTime` and Expired Requests:**  How does removing expired requests affect `virtualTime`? Could it become out of sync with the actual queue state?
* **`Push` and Flow Capacity:** What happens if `Push` encounters `ErrInsufficientFlowCapacity`? What if all flows are at capacity?

Question 31: Design Document Diagram (queue_design.md):

Priority: High
Estimated Time: 1-2 hours
Current Status: Placeholder present.
Proposed Solution/Plan: Create a clear diagram illustrating request flow, component interactions, and queueing/bypass logic.
Coding Tasks: N/A
Documentation Updates: Add diagram to queue_design.md.

Question 32: Design Document Consistency and Clarity (queue_design.md):

Priority: Medium
Estimated Time: 2-4 hours
Current Status: Inconsistent terminology (e.g., "model" vs. "flow"), lacking code examples for key algorithms, insufficient implementation details in some sections.
Proposed Solution/Plan: Ensure consistent terminology, add code examples, and provide more implementation details (popper management, cleanup process, etc.).
Coding Tasks: N/A
Documentation Updates: Update queue_design.md for consistency and clarity.

Question 33: Scheduler and Queue Controller Interaction Clarification (queue_design.md):

Priority: High
Estimated Time: 2-4 hours
Current Status: Insufficient detail about the interaction between scheduler and queue controller.
Proposed Solution/Plan: Clarify scheduling decisions in relation to queueing, limitations of the decoupled approach, and mitigation strategies (e.g., aggregate metrics feedback).
Coding Tasks: (Potentially) Implement feedback mechanisms if needed.
Documentation Updates: Update queue_design.md to detail scheduler interaction.

Question 34: Performance and Scaling Details (queue_design.md):

Priority: Medium
Estimated Time: 4-8 hours
Current Status: Performance and Scaling section lacks concrete details about benchmarking scenarios, metrics, and optimization strategies.
Proposed Solution/Plan: Add detailed descriptions of benchmarking plans (scenarios, metrics, success criteria), dynamic popper adjustment, and queue optimization.
Coding Tasks: (Potentially) Implement dynamic popper adjustment.
Documentation Updates: Update queue_design.md with performance and scaling details.

Question 35: Future Work Expansion (queue_design.md):

Priority: Medium
Estimated Time: 4-8 hours
Current Status: Future Work section lacks sufficient detail.
Proposed Solution/Plan: Expand Future Work section with concrete plans and considerations for each area (dynamic request sizing, scheduler integration, advanced queue management).
Coding Tasks: N/A
Documentation Updates: Update queue_design.md with expanded Future Work section.

### Expiry and Cancellation Optimization (Low Priority)

* **Problem:**  The current expiry cleanup mechanism uses a periodic ticker, potentially introducing delays in removing cancelled or expired requests from the queue. This can lead to unnecessary resource consumption and slightly increased latency for subsequent requests.
* **Goal:** Optimize the expiry and cancellation handling to remove expired/cancelled requests more promptly.
* **Proposed Solution:** Instead of relying solely on the periodic ticker, incorporate a callback or notification mechanism triggered upon request cancellation or expiry.  This mechanism would signal the `RequestQueueController` or the queue itself to immediately remove the affected request.
* **Estimated Time:** 4 hours
* **Priority:** Low
* **Open Questions/Risks:**
    * How best to integrate the callback/notification mechanism into the existing request lifecycle and the `FairRequestQueue` implementation?
    * Potential performance overhead of the callback mechanism under high concurrency.
* **Implementation Notes:**
    * Explore using context cancellation signals or dedicated channels for notifications.
    * Benchmark the performance impact of the new mechanism compared to the existing ticker-based approach.

## Question 34: Mitigating Excessive Preemption from Large Requests

**Priority:** Medium (Deferred)
**Estimated Time:** 4-6 hours

**Problem:** A very large request could cause excessive preemption of smaller requests, leading to unfairness and potential performance degradation.

**Goal:** Mitigate the impact of large requests on preemption behavior.

**Deferred:** Yes. We'll revisit this after the core queuing logic is stable and thoroughly tested.

**Potential Mitigation Strategies (to be considered later):**

1. **Size-Limited Preemption:** Introduce a `maxPreemptionSize` limit in `FairRequestQueue.Push` to restrict the total size of requests that can be preempted for a single incoming request.

2. **Request Size Classes:** Divide requests into size classes (small, medium, large) and use separate queues or a bucketed queue for each class. Limit preemption within each size class.

3. **Reject Excessively Large Requests:** Set a maximum request size limit and reject requests exceeding this limit without attempting preemption.

4. **Adaptive Preemption Limit:** Dynamically adjust the `maxPreemptionSize` based on queue length, current stock, or other relevant metrics.

5. **Prioritize Preemption by Virtual Finish Time:** Preferentially preempt requests close to completion (***smaller*** `virtualFinishTime`) to reduce the total number of preemptions.

**Coding Tasks (Deferred):**

* Implement and benchmark the chosen mitigation strategy.
* Add unit tests for the chosen strategy.

**Documentation Updates (Deferred):**

* Document the chosen strategy and its rationale in `queue_design.md`.

## Design Document Updates (queue_design.md)

* **Overall Structure and Consistency:**
    * **Task:** Review and reorganize the document for better flow and clarity.  Ensure consistent use of terminology (e.g., "flow," "request," "model").  Provide clear definitions for key concepts and data structures.  Consider adding a table of contents. (Priority: High)
* **Detailed Algorithm Descriptions:**
    * **Task:**  Add detailed descriptions (with pseudocode or diagrams) of the WFQ algorithm, preemption algorithm, and stock management with hysteresis bands. Explain the rationale for each design choice and the expected behavior under various scenarios. (Priority: High)
* **Locking Strategy:**
    * **Task:**  Clearly document the locking strategy used in `FairRequestQueue` and `RequestQueueController`.  Explain the use of mutexes, read/write locks, and the rationale for choosing specific locking mechanisms. Discuss potential areas for optimization and the trade-offs involved.  Mention the global lock, flow-level locks, and the potential for removing certain locks where it would improve performance without introducing issues related to concurrent access. (Priority: High)
* **Error Handling:**
    * **Task:** Document the error handling strategy for the queuing system, including the types of errors that can occur, how they are handled, and the fail-open/fail-close behavior in different scenarios.  Include a list of error codes and their meanings. (Priority: Medium)
* **Metrics:**
    * **Task:** Provide a comprehensive list of metrics collected by the queuing system, including their names, labels, and descriptions. Explain how these metrics can be used for monitoring and performance analysis.  (Priority: Medium)
* **Configuration Options:**
    * **Task:** Document all configuration options for the queuing system (e.g., total capacity, flow capacity, number of poppers, etc.), their default values, and their impact on system behavior.  (Priority: Medium)
* **Code Examples:**
    * **Task:**  Include relevant code snippets in the design document to illustrate key algorithms and data structures. This will improve clarity and make it easier for readers to understand the implementation details. (Priority: Medium)
* **Diagrams:**
    * **Task:**  Add diagrams to illustrate the request flow, component interactions, queueing/bypass logic, WFQ algorithm, preemption algorithm, and stock management. Visual representations can significantly improve understanding of complex concepts. (Priority: Medium)
* **Future Work:**
    * **Task:** Expand the Future Work section with more concrete details and considerations for each item (dynamic request sizing, scheduler integration, advanced queuing algorithms, etc.). (Priority: Medium)
