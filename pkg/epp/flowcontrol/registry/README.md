# Flow Registry Implementation

This package contains the concrete implementation of the `FlowRegistry` system, which includes its configuration structures and constructor. It implements the interfaces defined in the `flowcontrol/ports` package.

## Configuration

A `FlowRegistry` instance is configured using the `FlowRegistryConfig` struct. This configuration defines the system's sharding strategy, global capacity limits, and the set of priority bands that govern all flow control decisions.

### `FlowRegistryConfig`

This is the top-level configuration for the entire registry.

| Field | Type | Description | Default |
| :--- | :--- | :--- | :--- |
| `InitialShardCount` | `uint` | The number of parallel workers (shards) to initialize. Must be between `MinShards` and `MaxShards`. | `1` |
| `MinShards` | `uint`| The minimum number of shards for dynamic scaling. | `1` |
| `MaxShards` | `uint`| The maximum number of shards for dynamic scaling. Setting to `1` disables scaling up. | `1` |
| `MaxBytes` | `uint64` | A global byte-size limit aggregated across all priority bands and shards. | `0` (ignored) |
| `PriorityBands` | `[]PriorityBandConfig` | The list of priority bands managed by the registry. **At least one is required.** | `nil` |

---

### `PriorityBandConfig`

This struct defines the configuration for a single priority band within the registry. The system iterates through bands from the lowest `Priority` number (highest priority) to the highest.

| Field | Type | Description | Default |
| :--- | :--- | :--- | :--- |
| `Priority` | `uint` | **Required.** The numerical priority level. Lower values are higher priority. | N/A |
| `PriorityName` | `string` | **Required.** A human-readable name for the band (e.g., "Critical", "Standard"). | N/A |
| `InterFlowDispatchPolicy` | `plugins.Registered...` | The name of the registered policy for selecting which *flow* to service next within this band. | `"BestHead"` |
| `InterFlowDisplacementPolicy` | `plugins.Registered...` | The name of the registered policy for selecting a victim *flow* from this band during displacement. | `"RoundRobinDisplacement"` |
| `IntraFlowDispatchPolicy` | `plugins.Registered...` | The default policy for selecting which *item* to dispatch from a flow's queue. Can be overridden per-flow. | `"FCFS"` |
| `IntraFlowDisplacementPolicy` | `plugins.Registered...` | The default policy for selecting a victim *item* from a flow's queue. Can be overridden per-flow. | `"Tail"` |
| `QueueType` | `plugins.Registered...`| The default `SafeQueue` implementation to use for flows in this band. | `"ListQueue"` |
| `MaxBytes` | `uint64` | The maximum total byte size for this specific priority band, aggregated across all shards. | `1 GB` |
