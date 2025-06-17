# Flow Control Framework

This document details the core components of the Flow Control framework, focusing on the abstractions and interfaces
crucial for developers intending to implement custom policy or queue plugins.

The Flow Control framework is designed with extensibility as a primary goal. Its core engine (`FlowController`)
orchestrates decisions made by pluggable components—`Policy` plugins (defining request handling) and `SafeQueue`
implementations (defining request storage and ordering)—configured and managed by a stateful `FlowRegistry`.

## The `SafeQueue` System: Flexible and Extensible Queuing

`SafeQueue` implementations are responsible for the stateful storage and ordering of `QueueItemAccessor` instances. The
interface is designed with a small set of distinct, purpose-built methods for mutation and inspection.

* **Core Mutating Operations:**
    * `Add(item)` and `Remove(handle)`: The standard methods for adding an item and removing a specific item by its
      handle.
    * `Cleanup(predicate PredicateFunc) ([]QueueItemAccessor, error)`: Provides a highly-performant, atomic
      "find-and-remove" operation. This is the primary mechanism for frequent, partial eviction tasks, such as removing
      expired items.
    * `Drain() ([]QueueItemAccessor, error)`: Provides an unambiguous, atomic method for emptying the entire queue. This
      is used for state migration tasks within the `FlowRegistry`. While this could be accomplished by `Cleanup` with a
      predicate that always returns `true`, a dedicated `Drain` method can be implemented more efficiently (e.g., by
      re-initializing the underlying data structure instead of iterating) and is semantically unambiguous.

* **Inspection Methods (from `framework.QueueInspectionMethods`):**
    * `Len()`, `ByteSize()`, `PeekHead()`, `PeekTail()` (gated by `CapabilityDoubleEnded`).

* **Advanced Inspection:**
    * `Scan(predicate PredicateFunc) ([]QueueItemAccessor, error)`: A powerful but potentially expensive read-only
      method for finding all items that match a predicate. Its use is explicitly gated by `CapabilityScannable`.

### `QueueCapability`: Managing Extensibility and Avoiding Interface Proliferation

Different queue implementations (e.g., a simple FIFO list, a min heap priority queue, a double-ended queue) offer
different functionalities (e.g., FIFO, priority ordering, and efficient tail access respectively). Defining distinct Go
interfaces for every possible combination of these features (e.g., `PriorityConfigurableDoubleEndedQueue`) would
lead to an unmanageable explosion of interface types.

The `QueueCapability` system is a key design choice for managing the diverse functionalities of `SafeQueue`
implementations.

1.  **Unified `SafeQueue` Interface:** All implementations conform to the single `SafeQueue` interface.
2.  **Capability Declaration:** Each `SafeQueue`, via its `Capabilities()` method, returns a slice of `QueueCapability`
    strings (e.g., `CapabilityFIFO`, `CapabilityDoubleEnded`) that explicitly declares its supported features.
3.  **Policy Requirements:** `Policy` plugins, via their `RequiredQueueCapabilities()` method, declare the specific
    capabilities they need to operate correctly.
4.  **Centralized Validation by `FlowRegistry`:** The `FlowRegistry` ensures that the chosen `SafeQueue` possesses all
    capabilities required by the policies associated with a flow, providing fail-fast configuration.

This approach maintains a stable core `SafeQueue` contract while allowing for rich feature diversity. It promotes
polymorphism (policies operate on `FlowQueueAccessor`, which abstracts the `SafeQueue`, relying on pre-validated
capabilities), simplifies evolution, and enhances system robustness through explicit dependency declaration and
centralized validation.

**Conceptual Categories of Capabilities:**
* **Structural:** Describe methods available on the queue (e.g., `CapabilityDoubleEnded` for `PeekTail`).
* **Behavioral:** Describe internal ordering logic (e.g., `CapabilityFIFO`, `CapabilityPriorityConfigurable`).
* **Convention:** Only `IntraFlowDispatchPolicy` instances should typically require *behavioral* capabilities, as they
  define the queue's dispatch ordering. Other policies should primarily depend on *structural* capabilities for
  inspection. This convention is reinforced through documentation and code review.

### Design Justification: Specialized Methods vs. a Generic "Visitor" Pattern

An alternative design would be to use a single, generic `List(predicate, callback)` method to unify `Cleanup`, `Drain`,
and `Scan`. This approach was deliberately avoided due to significant concurrency and safety risks:

1.  **Deadlock Risk:** A generic callback pattern that combines finding and acting on an item
  (`queue.List(predicate, queue.Remove)`) creates a classic deadlock scenario. The `List` method would hold a lock on
  the queue's data, then call the `Remove` callback, which would in turn attempt to acquire the *same lock*, freezing
  the goroutine. Go's standard mutexes are not re-entrant.
2.  **Race Conditions and Stale State:** A two-phase approach
  (`items, _ := queue.Scan(predicate); for item := range items { queue.Remove(item.Handle()) }`) introduces race
  conditions. An item could be dispatched by another goroutine between the `Scan` and `Remove` calls, leading to errors
  and complex defensive logic in the caller.

The specialized methods (`Cleanup`, `Drain`, `Scan`) provide clear, unambiguous contracts that can be implemented in a
performant, deadlock-free manner.

### Design Justification: The Value of `CapabilityScannable`

The Flow Control framework makes a strong design choice to tightly couple a queue's primary sorting order to its
`IntraFlowDispatchPolicy` via the `ItemComparator`. While excellent for performance on the dispatch hot path, this
creates a challenge for displacement.

A `IntraFlowDisplacementPolicy` often needs to select a victim based on criteria that are **orthogonal** to the queue's
primary dispatch order. For example, a queue might be sorted by **earliest deadline first** for dispatch (a temporal
concern), but the most effective displacement strategy might be to remove the **largest request by byte size** to make
the most space (a resource consumption concern).

The `Scan` method, gated by `CapabilityScannable`, provides the necessary escape hatch. It allows a displacement policy
to inspect the full queue based on arbitrary criteria, enabling more sophisticated victim selection strategies than
simply removing from the `Tail`.

### Design Justification: Handling Dynamic Priorities

The framework provides a flexible model for handling items whose priorities change while they are in a queue.

* **The Behavioral Promise (`CapabilityDynamicPriority`):** A queue advertising this promises that its `PeekHead()`
  method will endeavor to return the highest-priority item according to the `ItemComparator`, even if priorities change
  over time. *How* it achieves this is an implementation detail.

* **The Optional Trigger (`PriorityUpdater` Interface):** For queues that support more efficient, synchronous updates,
  the framework provides the optional `PriorityUpdater` interface. The controller can perform a type assertion to check
  if a `SafeQueue` implements this interface and call `UpdatePriority(handle)` to explicitly trigger a targeted
  re-ordering operation.

This two-part design provides maximum flexibility, allowing simple queues to fulfill the contract passively while
enabling the system to leverage the performance of more advanced implementations when available.

## The Policy System: Layered Decision-Making for Request Flow

The framework utilizes a two-tier, pluggable policy system to govern request dispatch and displacement. This allows for
fine-grained control over how different flows are prioritized and how individual requests within those flows are
ordered.

### Terminology: "Dispatch" vs. "Displacement"

The framework uses specific terms for its policy actions:

* **Dispatch:** Refers to selecting a request to be sent forward for processing.
* **Displacement:** Refers to selecting a *queued* request to be evicted from its queue to make space for a
  higher-priority incoming request.

**Justification for "Displacement":** The term "Displacement" was chosen as it accurately describes a lower-priority
  queued item being moved from its place to accommodate a higher-priority one.

* *"Preemption"* was considered as it is sometimes used in this context in queuing theory language, but it carries
  strong OS connotations of interrupting an *actively running* task, whereas our items are queued.
* *"Eviction"* was considered but is already used more broadly within the framework for removals due to TTL expiry or
  context cancellation, so "DisplacementPolicy" avoids overloading the term.
* *"Shedding"* was considered, but it typically refers to a broader, system-wide strategy of dropping or rejecting
  *incoming* load when the system is at or near its absolute capacity. "Displacement" more accurately describes the
  targeted removal of an already-queued item to make space for a specific new one.

### Design Justification: Two-Tier Policy Framework (`InterFlow` vs. `IntraFlow`)

The separation of policies into `InterFlow` (operating *across* different logical flows) and `IntraFlow` (operating
*within* a single flow's queue) is a foundational design choice.

* **`InterFlow...Policy`:** Makes strategic decisions about fairness *among competing flows*. (e.g., "Which flow in this
  priority band gets the next opportunity?").
* **`IntraFlow...Policy`:** Makes tactical decisions *within a single flow*. (e.g., "Which of this flow's requests
  should be dispatched next?").

This layered approach provides a clear separation of concerns and enables modular composition of strategies.

### Design Justification: Explicit Orchestration over Policy "Hinting"

The `FlowController` acts as the sole orchestrator of the two-policy decision process. An alternative "hinting
mechanism, where an `InterFlow...Policy` could influence the `IntraFlow...Policy`, was rejected to prioritize strong
decoupling, simplicity, and safety. The framework guarantees that a queue's declared `IntraFlow...Policy` is always
respected.

While the `QueueItemAccessor.OriginalRequest()` method exists as an "escape hatch" for truly exotic,
application-specific needs, it is not intended for general inter-policy communication. The two-tiered approach provides
a robust, sufficient, and maintainable foundation for the vast majority of use cases without the need for a complex and
potentially brittle hinting system.

### Design Justification: Asymmetric, Distinct Policy Interfaces

The framework defines four distinct policy interfaces (`InterFlowDispatchPolicy`, `IntraFlowDispatchPolicy`,
`InterFlowDisplacementPolicy`, `IntraFlowDisplacementPolicy`). While structurally similar in pairs (inter-flow policies
select queues; intra-flow policies select items), they are kept distinct rather than unified (e.g., into single
`InterFlowPolicy` and `IntraFlowPolicy` types that handle both dispatch and displacement selection).

**Rationale:**

1.  **Semantic Clarity and Type Safety:** Distinct names and types make the role of each policy unambiguous and prevent
    misconfiguration at compile time.
2.  **Specialized Role of `IntraFlowDispatchPolicy`:**  This policy is unique as it is responsible for vending the
    `ItemComparator` that defines the fundamental dispatch ordering for its flow's queue. This tight coupling of
    selection logic (`SelectItem`) and ordering definition (`Comparator`) ensures a cohesive and safe mechanism for
    intra-flow dispatch, making the policy a self-contained semantic unit (e.g., "FCFS" implies both the ordering and
    selection). It is a deliberate design choice for clarity, performance on the hot path (dispatch), and safety
    (preventing mismatch between ordering and selection).
3.  **Future Flexibility:** Distinct interfaces allow each of the four policy roles to evolve independently.
4.  **Bounded Proliferation:** The number of distinct policy roles is fixed at four, meaning this approach does not lead
    to an unmanageable explosion of interfaces. The benefits of semantic clarity and safety outweigh the presence of a
    few additional interface definitions.

#### Why is `ItemComparator` Not a Separate Plugin?

A "fully decoupled" model was considered where `ItemComparator` would be its own plugin type, configured alongside a
generic `IntraFlowDispatchPolicy`. This approach was rejected because, while theoretically pure, it introduces
significant practical downsides:

1.  **Increased Cognitive Load:** "Shortest Job First" is a single concept. Decoupling forces a user to configure two
    components (`"ShortestJobFirst"` `ItemComparator`  and `"Head"` `IntraFlowDispatchPolicy`) to achieve one goal,
    increasing the chance of misconfiguration.
2.  **Risk of Mismatch:** The decoupled model makes it possible to pair incompatible plugins, making the system less
    safe. Our design makes this impossible by binding the ordering logic (`Comparator`) directly to the dispatch
    selection logic (`SelectItem`).
3.  **Complex State Management:** For stateful policies (e.g., one based on SLO violation probability), decoupling
    creates ambiguity about where state lives. Our design preserves clear encapsulation by keeping the state and the
    logic that uses it within a single policy object.

By tightly coupling the `ItemComparator` to the `IntraFlowDispatchPolicy`, the framework makes a deliberate trade-off,
prioritizing semantic clarity, safety, and ease of use over theoretical purity.

### Policy Naming Conventions

While interface types are fully descriptive (e.g., `InterFlowDispatchPolicy`), registered implementations use briefer
names (e.g., `"RoundRobin"`, `"FCFS"`). The configuration context in which a plugin is used provides its full semantic
meaning.

The asymmetric design means policy names naturally reflect their specific roles:

* **`IntraFlowDispatchPolicy`** names are often semantically rich and tied to their `ItemComparator` (e.g., `"FCFS"`,
  `"EarliestDeadlineFirst-TTFT"`).
* **`InterFlow...Policy`** names typically describe a structural or algorithmic approach (e.g., `"RoundRobin"`,
  `"BestHead"`, `"LargestQueue-Bytes"`).
* **`IntraFlowDisplacementPolicy`** names describe the victim selection criteria. This logic can be a simple inverse of
  the dispatch order (e.g., selecting the `"Tail"` from a FIFO queue) or an entirely orthogonal concern (e.g., selecting
  the `"Largest-Bytes"` victim from a deadline-sorted queue, which would require `CapabilityScannable`).

This variation in naming conventions is a feature, providing clarity at a glance about a policy's function.

### The `ItemComparator`

The `ItemComparator` is vended by an `IntraFlowDispatchPolicy` and defines the precise item ordering for dispatch within
that flow. It provides `Func() ItemComparatorFunc` for the comparison logic and `ScoreType() string` as a descriptor for
the comparison's domain. This `ScoreType` is crucial for `InterFlowPolicies` to determine if it can safely and
meaningfully compare head items from queues managed by different policies (by ensuring their `ScoreType` strings are
identical).

### Declaring Policy Requirements for Queue Capabilities

All policy interfaces include a `RequiredQueueCapabilities() []QueueCapability` method. Policies must declare the
`SafeQueue` capabilities they need to function, which the `FlowRegistry` uses to validate configurations at startup.

### Future Considerations: A Pluggable Inter-Band Dispatch Policy

The initial design uses a strict-priority model for iterating through priority bands. This is a simple, predictable, and
robust model sufficient for a wide range of use cases. A potential future enhancement is the introduction of a pluggable
`InterBandDispatchPolicy`. This would enable replacing the strict-priority loop with more flexible policies (e.g.,
Weighted Fair Queuing), which would solve the primary limitation of the current model: the potential for a busy,
high-priority band to completely **starve** all lower-priority bands. The framework's core components have been designed
to be compatible with this future addition.

A corresponding `InterBandDisplacementPolicy` was also considered for architectural symmetry. However, this idea was
rejected. The current hardcoded logic—always targeting the absolute lowest-priority band for victims first—is simple,
intuitive, and aligns with the most common fairness principle for displacement. The inability to identify compelling,
alternative strategies that would justify the added complexity of a pluggable policy makes the simpler, hardcoded
approach superior.

---

**Contribution Guides:**

* For implementing `InterFlowDispatchPolicy`: TODO `[Link to ./plugins/interflow/dispatch/README.md]`
* For implementing `IntraFlowDispatchPolicy`: TODO `[Link to ./plugins/intraflow/dispatch/README.md]`
* For implementing `InterFlowDisplacementPolicy`: TODO `[Link to ./plugins/interflow/displacement/README.md]`
* For implementing `IntraFlowDisplacementPolicy`: TODO `[Link to ./plugins/intraflow/displacement/README.md]`
* For implementing `SafeQueue`: TODO `[Link to ./plugins/queue/README.md]`

