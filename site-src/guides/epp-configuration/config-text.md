# EPP Configuration (YAML)

The Endpoint Picker (EPP) is configured via a YAML file (or inline text).
This configuration defines the behavioral logic of the gateway:

1.  **Plugins:** The modular components to load (Scorers, Controllers, Connectors, Hooks).
2.  **Scheduling Profiles:** How scheduling plugins are composed into routing pipelines.
3.  **Feature Gates:** Which experimental features to enable.

***NOTE***: Although this configuration resembles a Kubernetes CRD, it is **static**. It is read only at startup;
changes to this file require restarting the EPP to take effect.

### Architecture & Extension Points

The Endpoint Picker (EPP) is a modular system composed of **Plugins**. To configure them correctly, it is helpful to
distinguish between the four main categories:

1.  **Scheduling Plugins (The "Which Pod?" Decision)**
    These execute **per-request** to rank and select backends. They form a pipeline (Filter &rarr; Score &rarr; Pick)
    and **must** be referenced in a **Scheduling Profile** to take effect.
    *   **Filters:** Exclude backends.
    *   **Scorers:** Rank backends (e.g., `PrefixCacheScorer`).
    *   **Pickers:** Select the winner (e.g., `MaxScorePicker`).

2.  **Flow Control Plugins (System Protection)**
    These operate as global controllers or gatekeepers to protect the system from overload.
    They are typically active simply by being defined in the `plugins` list; they do not need to be added to a
    scheduling profile.
    *   **Saturation Controller:** Monitors backend metrics (Queue Depth, KV Cache) to detect overload.

3.  **Request Lifecycle Plugins (Interception & Hooks)**
    These hooks execute at specific points in the request path to perform logic, logging, or data modification.
    They are automatically registered into the request path upon instantiation.
    *   **Admission:** Validates requests before scheduling (e.g., `AdmissionPlugin`).
    *   **Data Prep:** Enriches requests with necessary data (e.g., `PrepareDataPlugin`).
    *   **Hooks:** `PreRequest`, `ResponseReceived`, `ResponseStreaming`, and `ResponseComplete` hooks for custom logic.

4.  **Data Layer Plugins (Observability)**
    These manage the ingestion of metrics from model servers, feeding the data used by other plugins.
    *   **Data Sources:** Connectors for scraping or receiving backend metrics.

### Structure

The configuration follows this structure:

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha1
kind: EndpointPickerConfig
plugins:
  # Instantiates plugins with specific parameters
  - name: my-scorer
    type: prefix-cache-scorer
    parameters: { ... }
schedulingProfiles:
  # Defines how to use the instantiated scheduling plugins
  - name: default
    plugins:
      - pluginRef: my-scorer
        weight: 50
featureGates:
  # Enables experimental features
  - flowControl
```

### 1. Configuring Plugins

The `plugins` section defines which logic components are loaded.

*   **name** (Optional): A unique alias for this instance. If omitted, the `type` is used as the name.
*   **type**: The identifier of the plugin implementation (see [Plugin Reference](#plugin-reference)).
*   **parameters** (Optional): Key-value pairs specific to that plugin type.

### 2. Configuring Scheduling Profiles

The `schedulingProfiles` section defines routing behavior. You can define multiple profiles (e.g., one for prefill, one
for decode), though a single `default` profile is sufficient for most use cases.

*   **name**: The unique name of the profile.
*   **plugins**: A list of references to instantiated plugins.
    *   **pluginRef**: Matches the `name` (or `type`) defined in the `plugins` section.
    *   **weight** (Optional): Used for Scorers. Defaults to `1`.

**Defaults & Behavior:**
*   If no `schedulingProfiles` are defined, a `default` profile is automatically created using all loaded plugins.
*   If no **Picker** is referenced in a profile, a `MaxScorePicker` is automatically added.
*   If only one profile exists, the `SingleProfileHandler` is automatically used.

### Example Configuration

Passing configuration via a file:
```yaml
args:
  - --config-file
  - "/etc/epp/epp-config.yaml"
```

Passing configuration inline:
```yaml
args:
  - --config-text
  - |
    apiVersion: inference.networking.x-k8s.io/v1alpha1
    kind: EndpointPickerConfig
    plugins:
    - type: prefix-cache-scorer
      parameters:
        blockSize: 5
    - type: saturation-controller
      parameters:
        queueDepthThreshold: 10
    schedulingProfiles:
    - name: default
      plugins:
      - pluginRef: prefix-cache-scorer
        weight: 50
    featureGates:
    - flowControl
```

---

### Plugin Reference

#### **SingleProfileHandler**
Determines which profile to use for a request.
*   **Type**: `single-profile-handler`
*   **Parameters**: None.
*   *Note:* Automatically enabled if only one profile is defined.

#### **PrefixCacheScorer**
Scores pods based on estimated KV-cache hits (common prefix matching).
*   **Type**: `prefix-cache-scorer`
*   **Parameters**:
    *   `blockSize` (int, default: 64): Token block size for hashing.
    *   `maxPrefixBlocksToMatch` (int, default: 256): Limit on matching depth.
    *   `lruCapacityPerServer` (int, default: 31250): Size of the LRU index per backend.

#### **KvCacheScorer**
Scores pods based on current KV cache utilization (lower usage = higher score).
*   **Type**: `kv-cache-utilization-scorer`
*   **Parameters**: None.

#### **QueueScorer**
Scores pods based on waiting queue size (shorter queue = higher score).
*   **Type**: `queue-scorer`
*   **Parameters**: None.

#### **LoraAffinityScorer**
Scores pods higher if they already have the requested LoRA adapter loaded.
*   **Type**: `lora-affinity-scorer`
*   **Parameters**: None.

#### **MaxScorePicker**
Selects the pod with the highest total score.
*   **Type**: `max-score-picker`
*   **Parameters**:
    *   `maxNumOfEndpoints` (int, default: 1): Number of top candidates to select.

#### **RandomPicker**
Selects a random pod from the candidates.
*   **Type**: `random-picker`
*   **Parameters**:
    *   `maxNumOfEndpoints` (int, default: 1).

#### **WeightedRandomPicker**
Selects pods using weighted random sampling (A-Res algorithm) based on scores.
*   **Type**: `weighted-random-picker`
*   **Parameters**:
    *   `maxNumOfEndpoints` (int, default: 1).

#### **StaticThresholdSaturationController**
Acts as the gatekeeper for the Flow Control system. It monitors backend capacity signals to prevent overload.
*   **Type**: `static-threshold-saturation-controller`
*   **Parameters**:
    *   `queueDepthThreshold` (int, default: 5): The target waiting queue size on the backend.
        * **> 0 (Throughput Mode):** Allows local buffering to maximize batch size/GPU utilization.
        * **0 (Latency Mode):** Forces Just-In-Time dispatching for strict priority/fairness.
    *   `kvCacheUtilThreshold` (float, default: 0.8): The safety ceiling (0.0-1.0) for KV cache usage.
    *   `metricsStalenessThreshold` (duration, default: 200ms): The maximum age of metrics before a backend is
        considered unsafe.

---

### Feature Gates

Enables experimental functionality.

*   `dataLayer`: Enables the experimental Data Layer APIs.
*   `flowControl`: Enables the Flow Control and Saturation logic.
