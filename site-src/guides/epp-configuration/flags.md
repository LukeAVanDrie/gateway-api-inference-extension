# EPP Configuration Flags

This page documents specific configuration flags for the Endpoint Picker (EPP) binary. While most flags are
self-explanatory via the `--help` output, the flags detailed below have nuanced behavior regarding discovery and
backward compatibility.

## Runtime Identity

### `--pool-namespace`

**Description:**
Specifies the Kubernetes namespace of the InferencePool managed by this EPP instance.

**Resolution Order:**
The EPP determines its namespace using the following precedence:
1.  **Flag:** Uses the value of `--pool-namespace` if explicitly set.
2.  **Env:** Falls back to the `NAMESPACE` environment variable if the flag is omitted.
3.  **Default:** Defaults to `default` if neither are present.

**Best Practice:**
Leave this flag unset and inject the `NAMESPACE` environment variable via the Kubernetes Downward API. This allows the
EPP to automatically discover its own namespace, making your deployment manifests portable across environments.

**Example: Injecting Namespace via Downward API**

```yaml
env:
  - name: NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
```

---

## Deprecated Configuration

The following configuration methods are **deprecated** and will be removed in a future release. Users are strongly
encouraged to migrate to the [Text-Based Configuration](./config-file.md) format.

### Environment Variables

The use of environment variables for core logic configuration is deprecated.

| Deprecated Env Var | Replacement in YAML Config |
| :--- | :--- |
| `SD_QUEUE_DEPTH_THRESHOLD` | Plugin `SaturationController` parameter: `queueDepthThreshold` |
| `SD_KV_CACHE_UTIL_THRESHOLD` | Plugin `SaturationController` parameter: `kvCacheUtilThreshold` |
| `SD_METRICS_STALENESS_THRESHOLD` | Plugin `SaturationController` parameter: `metricsStalenessThreshold` |
| `ENABLE_EXPERIMENTAL_FLOW_CONTROL_LAYER` | `featureGates: ["flowControl"]` |
| `ENABLE_EXPERIMENTAL_DATALAYER_V2` | `featureGates: ["dataLayer"]` |

### Legacy Metric Flags

Direct configuration of backend scraping via CLI flags is deprecated in favor of the **Data Layer v2** configuration.

| Deprecated Flag | Migration Path |
| :--- | :--- |
| `--model-server-metrics-port` | Configure a `metrics` source in the YAML config file. |
| `--model-server-metrics-path` | Configure a `metrics` source in the YAML config file. |
| `--model-server-metrics-scheme` | Configure a `metrics` source in the YAML config file. |
| `--total-queued-requests-metric` | Configure a `metrics` extractor in the YAML config file. |
| `--kv-cache-usage-percentage-metric` | Configure a `metrics` extractor in the YAML config file. |

---

For a complete list of all available flags, run:

```bash
EPP_BINARY --help
```
