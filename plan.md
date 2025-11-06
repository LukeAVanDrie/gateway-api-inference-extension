# Flow Control Plugin Migration Plan

## Goal

Refactor the Flow Control framework components (`SafeQueue`, `ItemComparator`, `IntraFlowDispatchPolicy`, `InterFlowDispatchPolicy`) to align with the EPP Plugin Model, making them configurable and extensible via the EPP YAML configuration, utilizing the central `pkg/epp/plugins` registry.

## Tracking

- [ ] Migrate `SafeQueue` Implementations
- [ ] Refactor `ItemComparator` into Plugins
- [ ] Migrate `IntraFlowDispatchPolicy` Implementations
- [ ] Implement `GenericPriorityPolicy`
- [ ] Migrate `InterFlowDispatchPolicy` Implementations
- [ ] Update Tests
- [ ] Update Documentation

## Detailed Steps

1.  **Migrate `SafeQueue` Implementations (`plugins/queue/`)**
    - [ ] **`listqueue/listqueue.go`**:
        - [ ] Ensure `listQueue` struct embeds `plugins.Plugin` and has `TypedName()`.
        - [ ] Define `ListQueueFactory` matching `func(string, json.RawMessage, plugins.Handle) (plugins.Plugin, error)`.
        - [ ] `init()` calls `plugins.Register("ListQueue", ListQueueFactory)`.
    - [ ] **`maxminheap/maxminheap.go`**:
        - [ ] Ensure `maxMinHeap` struct embeds `plugins.Plugin` and has `TypedName()`.
        - [ ] Define `MaxMinHeapFactory` to parse `comparatorRef` from parameters. Use `handle.Plugin()` to fetch the named `ItemComparator` plugin and type assert to `framework.ItemComparator`.
        - [ ] `init()` calls `plugins.Register("MaxMinHeap", MaxMinHeapFactory)`.
    - [ ] Delete `plugins/queue/factory.go` and its tests if they exist.

2.  **Refactor `ItemComparator` into Plugins (`plugins/comparators/`)**
    - [ ] Create directory `pkg/epp/flowcontrol/framework/plugins/comparators/`.
    - [ ] **`enqueue_time/enqueue_time.go`**:
        - [ ] Define `EnqueueTimeComparator` struct, embed `plugins.Plugin`, add `TypedName()`.
        - [ ] Implement `framework.ItemComparator` interface.
        - [ ] Define `EnqueueTimeComparatorFactory`.
        - [ ] `init()` calls `plugins.Register("EnqueueTime", EnqueueTimeComparatorFactory)`.

3.  **Migrate `IntraFlowDispatchPolicy` Implementations (`plugins/policies/intraflow/dispatch/`)**
    - [ ] **`fcfs/fcfs.go`**:
        - [ ] Ensure struct embeds `plugins.Plugin`, has `TypedName()`.
        - [ ] Define `FCFSFactory`. Factory uses `handle.Plugin("EnqueueTime")` to fetch the `ItemComparator`.
        - [ ] `init()` calls `plugins.Register("FCFS", FCFSFactory)`.
    - [ ] **`genericpriority/genericpriority.go`**:
        - [ ] Create this new file.
        - [ ] Implement `GenericPriorityPolicy` as `framework.IntraFlowDispatchPolicy` & `plugins.Plugin`.
        - [ ] Factory parses `comparatorRef` from parameters, uses `handle.Plugin()` to fetch the `ItemComparator`.
        - [ ] Factory parses `queueRef` and uses `handle.Plugin()` to fetch the `SafeQueue`. Validate queue capabilities against `framework.CapabilityPriorityConfigurable`.
        - [ ] `init()` calls `plugins.Register("GenericPriority", GenericPriorityFactory)`.
    - [ ] Delete `plugins/policies/intraflow/dispatch/factory.go` and tests.

4.  **Migrate `InterFlowDispatchPolicy` Implementations (`plugins/policies/interflow/dispatch/`)**
    - [ ] **`besthead/besthead.go`** & **`roundrobin/roundrobin.go`**:
        - [ ] Ensure structs embed `plugins.Plugin`, have `TypedName()`.
        - [ ] Define local factories.
        - [ ] `init()` calls `plugins.Register` with appropriate names.
    - [ ] Delete `plugins/policies/interflow/dispatch/factory.go` and tests.

5.  **Update Tests**
    - [ ] Modify unit tests to test factory functions, mocking `plugins.Handle` to return mock dependencies.
    - [ ] Update functional tests to use the central `plugins.Registry` to get instances for testing.

6.  **Update Documentation**
    - [ ] Update READMEs in the framework directories.
    - [ ] Update the EPP Configuration guide to detail the new flow control plugin types and their parameters (e.g., `comparatorRef`, `queueRef`).
