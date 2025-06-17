# Flow Control Framework Plugins

This package serves as the central catalog and registration point for all concrete `Policy` and `SafeQueue`
implementations available within the Flow Control framework. It provides the necessary types and constants for
configuring the `FlowRegistry`.

The actual plugin implementations will reside in subpackages:
* `./queue`: Contains `framework.SafeQueue` implementations.
* `./interflow/dispatch`: Contains `framework.InterFlowDispatchPolicy` implementations.
* `./interflow/displacement`: Contains `framework.InterFlowDisplacementPolicy` implementations.
* `./intraflow/dispatch`: Contains `framework.IntraFlowDispatchPolicy` implementations.
* `./intraflow/displacement`: Contains `framework.IntraFlowDisplacementPolicy` implementations.

## Plugin Registration and Naming

Each plugin implementation is identified by a unique registration name (e.g., `"ListQueue"`). The
`registry.FlowRegistryConfig` uses these names to construct the required policies and queues. The canonical
`Registered...Name` types and constants for all built-in plugins are defined in the `catalog.go` file.

## Available Built-in Plugins

The following tables list the standard, out-of-the-box plugins provided by the framework.

### Queue Plugins (`RegisteredQueueName`)

| Registered Name | Description | Capabilities Provided |
| :--- | :--- | :--- |
| `"ListQueue"` | A simple, double-ended queue implementation based on `container/list`. | `FIFO`, `DoubleEnded` |

### Inter-Flow Dispatch Policies (`RegisteredInterFlowDispatchPolicyName`)

| Registered Name | Description | Required Queue Capabilities | Requires Compatible Comparators |
| :--- | :--- | :--- | :--- |
| `"BestHead"` | Selects the flow queue whose head item has the highest priority. | None | `true` |
| `"RoundRobinDispatch"` | Selects flow queues in a simple round-robin order to ensure basic fairness. | None | `false` |

### Inter-Flow Displacement Policies (`RegisteredInterFlowDisplacementPolicyName`)

| Registered Name | Description | Required Queue Capabilities | Requires Compatible Comparators |
| :--- | :--- | :--- | :--- |
| `"WorstTail"` | Selects the flow queue whose tail item has the lowest priority. | `DoubleEnded` | `true` |
| `"RoundRobinDisplacement"` | Selects victim flow queues in a simple round-robin order to ensure basic fairness. | None | `false` |

### Intra-Flow Dispatch Policies (`RegisteredIntraFlowDispatchPolicyName`)

| Registered Name | Description | Required Queue Capabilities  |
| :--- | :--- | :--- |
| `"FCFS"` | "First-Come, First-Served". A standard FIFO ordering based on item enqueue time. | `FIFO` |

### Intra-Flow Displacement Policies (`RegisteredIntraFlowDisplacementPolicyName`)

| Registered Name | Description | Required Queue Capabilities |
| :--- | :--- | :--- |
| `"Tail"` | Selects the item at the tail of the queue as the victim. | `DoubleEnded` |

## Custom Plugins

To add a custom plugin to the framework:

1.  **Implement the Interface:** In a new package, create a struct that implements the relevant `framework` interface
    (e.g., `framework.SafeQueue`).
2.  **Register the Implementation:** In an `init()` function within your new package, register an instance of your
    implementation with the appropriate central factory, providing a unique string name.
3.  **Configure:** Use that unique name in your `FlowRegistryConfig` to activate the plugin for a given priority band or
    flow.
