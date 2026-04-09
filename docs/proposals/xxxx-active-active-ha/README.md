# Prefix Indexer State Replication Across EPP Replicas

## 1. Background

### 1.1 The Problem

The EPP (Endpoint Picker) maintains an **approximate prefix index** — an in-memory data structure that tracks which prefix hashes are likely cached on which model server (vLLM pod). This enables prefix-cache-aware scheduling: routing requests to servers that already have matching prefix tokens cached in GPU HBM, reducing Time to First Token (TTFT).

When moving to **active-active** (multiple EPP replicas processing traffic concurrently), each replica builds its own prefix index independently, losing the global view. The proposal explicitly identifies this as the main drawback:

The performance degradation has been proved by experiments [TODO: add link to experiment results]

### 1.2 The Indexer Data Structure

The indexer maintains two maps protected by a `sync.RWMutex`:

```go
type indexer struct {
    mu             sync.RWMutex
    hashToPods     map[BlockHash]podSet                         // reverse index: hash → set of servers
    podToLRU       map[ServerID]*lru.Cache[BlockHash, struct{}] // per-server LRU cache
    defaultLRUSize int
}
```

Each model server gets a bounded LRU cache (default capacity: 31,250 entries, matching ~500K tokens on H100 80GB with llama3-8B). When the LRU is full, adding a new entry evicts the least-recently-used entry and triggers a callback that cleans up `hashToPods`.

### 1.3 How the Indexer Is Updated

After scheduling a request, `PreRequest` calls `indexer.Add(state.PrefixHashes, server)` asynchronously.

The `Add` method inserts hashes into the per-server LRU (potentially evicting old entries) and updates the reverse index.

### 1.4 How the Indexer Is Queried

During scoring, `matchLongestPrefix` iterates through the request's prefix hashes and looks up which servers have each hash cached. The score for each endpoint is `matchLen / total`.

---

## 2. Mathematical Model: Why Syncing Eviction Events is Unnecessary and CRDTs Are Not Required

### 2.1 Definitions

Let:
- $s$ be a model server
- $e$ be an EPP replica
- $C$ be the LRU capacity for server $s$ (number of GPU blocks)
- $P$ be the maximum number of prefix blocks hashed and tracked by the EPP (`maxPrefixBlocks`, e.g., $P = 256$)
- $L$ be a random variable representing the total prompt length of a request (in blocks)
- $p = P(L > P)$ be the probability that a request's length exceeds $P$ (typically small, e.g., 10%)
- $E[L | L > P]$ be the expected length of requests greater than $P$ (e.g., $512$)
- $E[L | L \le P]$ be the expected length of requests less than or equal to $P$
- $B_{GPU}$ be the number of blocks added to the GPU cache per request
- $B_{EPP}$ be the number of blocks added to the EPP LRU per request
- $n$ be the number of types of shared prefixes
- $l_i$ be the length of the $i$-th shared prefix (in blocks)
- $p_i$ be the probability that a request contains the $i$-th shared prefix

### 2.2 The Indexer Is Already an Approximation

The fundamental property of the system is that **no EPP replica ever has exact knowledge of the true cache state**. Even with a single EPP, the view is approximate because:
1. The EPP mimics vLLM's LRU eviction but cannot observe vLLM's actual eviction decisions
2. vLLM may evict entries due to memory pressure from running requests (not modeled by EPP)
3. Only the first maxPrefixBlocks are hashed and added to the lruForPod. 

### 2.3 Why Syncing Eviction Events is Unnecessary

A key operation in prefix caching is eviction. One might assume that active-active EPP replicas need to synchronize eviction events to maintain consistent indices. However, we can show that the EPP's LRU naturally lags behind the GPU's actual eviction, making eviction synchronization redundant.

#### 2.3.1 Mathematical Proof

When a request is processed, it adds **new** blocks to the cache. Blocks that are already part of a shared prefix do not consume new capacity in either the GPU cache (due to sharing) or the EPP LRU (due to hash deduplication).

**Expected New Cache Consumption Per Request**

Let $I$ be the index of the matched shared prefix for a request (if any, with $l_I = 0$ for no match).

1. The **GPU cache** stores the KV states for the entire prompt. The expected number of *new* blocks added to the GPU cache per request is:
   
   $$E[B_{GPU}^{new}] = E[L - l_I] = E[L] - \sum_{i=1}^n p_i l_i$$
   
   Let $S_{GPU} = \sum_{i=1}^n p_i l_i$ be the expected shared length on the GPU. Thus, $E[B_{GPU}^{new}] = E[L] - S_{GPU}$.

2. The **EPP LRU** only hashes and tracks at most the first $P$ blocks. The expected number of *new* blocks added to the EPP LRU per request is:
   
   $$E[B_{EPP}^{new}] = E[\min(L, P) - \min(l_I, P)] = E[\min(L, P)] - \sum_{i=1}^n p_i \min(l_i, P)$$
   
   Let $S_{EPP} = \sum_{i=1}^n p_i \min(l_i, P)$ be the expected shared length tracked by the EPP. Thus, $E[B_{EPP}^{new}] = E[\min(L, P)] - S_{EPP}$.

**Rate of Eviction**

Let $N_{GPU}$ be the expected number of requests to fill the GPU cache and trigger eviction, and $N_{EPP}$ be the same for the EPP LRU:

$$N_{GPU} = \frac{C}{E[B_{GPU}^{new}]}, \quad N_{EPP} = \frac{C}{E[B_{EPP}^{new}]}$$

We want to compare $E[B_{GPU}^{new}]$ and $E[B_{EPP}^{new}]$.

**Analysis**

**Case A: Typical Scenario ($l_i \le P$ for all $i$)**

If all shared prefixes fit within the tracking limit $P$:
Then $\min(l_i, P) = l_i$, which implies $S_{GPU} = S_{EPP}$.

The difference in expected new blocks remains:

$$E[B_{GPU}^{new}] - E[B_{EPP}^{new}] = E[L] - E[\min(L, P)] = \Delta > 0$$

However, because the base consumption is smaller due to sharing, the ratio of requests before eviction increases:

$$\frac{N_{EPP}}{N_{GPU}} = \frac{E[L] - S_{GPU}}{E[\min(L, P)] - S_{GPU}}$$

Since $E[L] > E[\min(L, P)]$ and we subtract a positive $S_{GPU}$ from both, this ratio is strictly greater than the non-sharing ratio $\frac{E[L]}{E[\min(L, P)]}$.

**Case B: General Scenario (Some $l_i > P$)**

For requests matching a prefix with $l_i > P$, the GPU still adds $L - l_i \ge 0$ blocks, while the EPP adds $0$. Thus, $E[B_{GPU}^{new}] \ge E[B_{EPP}^{new}]$ holds in all cases.

This proves that the GPU cache fills up and starts evicting entries *sooner* than the EPP LRU, and prefix sharing amplifies this effect.

#### 2.3.2 Numerical Example

Using typical values:
- $C = 31{,}250$, $P = 256$, $p = 0.1$, $E[L | L > P] = 512$.
- Assume $E[L | L \le P] = 128$ (average length of small requests is half of $P$).
- **New**: Assume all requests share a common system prompt of length $100$ blocks ($n=1, p_1=1.0, l_1=100$).

Then the expected shared length is $S_{GPU} = S_{EPP} = 100$.

The base expectations (without sharing) would be:
- $E[L] = (0.9 \times 128) + (0.1 \times 512) = 166.4$ blocks.
- $E[\min(L, P)] = (0.9 \times 128) + (0.1 \times 256) = 140.8$ blocks.

Accounting for sharing, the expected *new* blocks added per request are:
- $E[B_{GPU}^{new}] = 166.4 - 100 = 66.4$ blocks.
- $E[B_{EPP}^{new}] = 140.8 - 100 = 40.8$ blocks.

Requests to fill the cache:
- $N_{GPU} = 31{,}250 / 66.4 \approx 470.6$ requests.
- $N_{EPP} = 31{,}250 / 40.8 \approx 765.9$ requests.

Ratio: $765.9 / 470.6 \approx 1.62$.

The EPP LRU holds entries for about 62% more requests than the GPU can actually hold. Thus, the EPP's view is way more optimistic, invalidating the need to synchronize eviction events.

### 2.4 Why CRDTs Are Not Required

Since we established in Section 2.3 that eviction events do not need to be synchronized, the only remaining state updates are `Add` events. The only source of divergence between EPP replicas is the **order** in which they receive these `Add` events.

If Replica A receives $Add(X)$ then $Add(Y)$, while Replica B receives $Add(Y)$ then $Add(X)$ due to network delays, their LRU states will differ slightly regarding the specific LRU order of $X$ and $Y$.

However, this ordering divergence is negligible for two reasons:

1. **It is bounded by network delay**: In a cluster processing $r=100$ requests/second with a maximum network delay of $\Delta t = 100\text{ms}$, at most $r \cdot \Delta t = 10$ requests can be reordered at any time. As calculated in Section 2.3.2, the EPP cache holds entries for approximately $N_{EPP} \approx 765$ requests. Reordering $10$ requests affects at most 10 / 765 = 1.3% of the cache's request capacity.
2. **The indexer is already approximate**: Due to reasons outlined in Section 2.2, the EPP's view of the cache inherently drifts from the true GPU state by a much larger value. A 1.3% divergence between replicas is noise in comparison.

A Conflict-Free Replicated Data Type (CRDT) like a Last-Writer-Wins Set (LWW-Set) would eliminate this 1.3% divergence by enforcing a total order or deterministically resolving conflicts. However, it would do nothing to reduce the much larger inherent approximation error.

Therefore, the high engineering complexity of integrating CRDTs (custom data structures, conflict resolution logic) is not justified. Simple, unordered `Add` event propagation is sufficient.

### 2.5 What Replication Actually Buys Us

The primary value of replication is **awareness**, not **consistency**. Without replication:

- $e_1$ knows about requests it routed, but not requests $e_2$ or $e_3$ routed
- Each replica's index covers only $\frac{1}{N}$ of the total request volume (where $N$ = number of active replicas)

With simple `Add` event replication (no ordering guarantees):

- All replicas know about **all** scheduling decisions across all replicas
- Each replica's index covers n/n = 100% of the request volume

The jump from $\frac{1}{N}$ coverage to full coverage is the dominant improvement. 

---

## 3. Design Decision

**Go with simple `Add` event replication with no ordering guarantees, and accept the LRU divergence.**

The indexer is already approximate by design. The main win — all EPPs knowing about all servers' recent prefixes — does not require consistent eviction state. The inter-replica LRU divergence is negligible compared to the inherent approximation error of the indexer itself.

---

## 4. Implementation Details

### 4.1 Member Discovery

**Mechanism**: Headless Service + EndpointSlice watch via controller-runtime.

EPP pods are already deployed behind a Service with label selectors. The existing EPP Service uses `type: ClusterIP`

For peer discovery, create an additional **headless Service** (`clusterIP: None`) for the EPP pods on a dedicated peer-sync port (e.g., 9004):

```yaml
apiVersion: v1
kind: Service
metadata:
  name: vllm-llama3-8b-instruct-epp-peer
spec:
  clusterIP: None
  selector:
    app: vllm-llama3-8b-instruct-epp
  ports:
    - name: peer-sync
      port: 9004
      targetPort: 9004
      protocol: TCP
```

Each EPP pod watches the EndpointSlice for this headless Service using controller-runtime's client (already a dependency). When endpoints change (pod added/removed), the peer list is updated. The project already uses EndpointSlice watching patterns in its test infrastructure.

Each pod filters itself out of the peer list using the `POD_NAME` and `NAMESPACE` environment variables already injected into every EPP pod.

### 4.2 Event Streaming

**Mechanism**: gRPC bidirectional streaming between peers.

gRPC is already a core dependency (`google.golang.org/grpc v1.78.0`).

Define a peer sync gRPC service:

```protobuf
syntax = "proto3";
package prefixsync.v1;

service PrefixSync {
  // Bidirectional stream for ongoing event replication
  rpc StreamEvents(stream PrefixEventBatch) returns (stream PrefixEventBatch);
  
  // Full state snapshot for new/restarting pods
  rpc SnapshotSync(SnapshotRequest) returns (stream PrefixSnapshotBatch);
}

message PrefixEventBatch {
  repeated PrefixEvent events = 1;
}

message PrefixEvent {
  repeated uint64 hashes = 1;
  string server_name = 2;
  string server_namespace = 3;
  int32 num_gpu_blocks = 4;
}

message SnapshotRequest {}

message PrefixSnapshotBatch {
  // Per-server entries, ordered from most-recently-used to least-recently-used
  string server_name = 1;
  string server_namespace = 2;
  int32 num_gpu_blocks = 3;
  repeated uint64 hashes = 4;  // MRU-ordered
}
```

**Batching**: Buffer `Add` events for a configurable window (e.g., 50-100ms) before sending. This reduces per-RPC overhead from potentially 100K individual RPCs to ~1K-2K batched messages. Each `Add` call can contain up to 256 hashes (`MaxPrefixBlocksToMatch` default), so batching is important for throughput.

**Registration**: Register the peer sync gRPC server as a `Runnable` on the controller-runtime manager, following the established pattern. 

The `internal/runnable` package already provides a `GRPCServer` helper that handles listening, graceful shutdown, and context cancellation.

**Connection lifecycle**: Each EPP pod maintains persistent gRPC streams to all known peers. When a peer is discovered (via EndpointSlice watch), open a `StreamEvents` connection. When a peer disappears, close the stream. With 3 pods, each pod maintains exactly 2 outbound streams.

### 4.3 Snapshot Sync

**When**: A new or restarted EPP pod, after discovering peers, requests a full state snapshot from one peer via the `SnapshotSync` RPC.

**Sending the snapshot**: The responding peer iterates through its `podToLRU` map and for each server, streams the LRU entries in MRU (most-recently-used) order. The `hashicorp/golang-lru` `Keys()` method returns keys in order from oldest to newest, so the sender reverses the order.

**Applying the snapshot**: The receiving pod calls `indexer.Add(hashes, server)` for each batch. Since the local LRU starts empty, there are no eviction conflicts during snapshot application — the LRU has plenty of capacity. The MRU ordering ensures that if the snapshot is larger than the local LRU capacity, the most valuable (most recently used) entries are retained.

**Data volume**: With `DefaultLRUCapacityPerServer = 31,250` entries per server and ~10 servers, the maximum snapshot size is ~312,500 entries. Each entry is a `uint64` hash (8 bytes) + server identity (~50 bytes overhead in protobuf). Total: **~18MB**, transferable in well under a second over gRPC.

After the snapshot completes, the pod switches to receiving streamed events from all peers (the ongoing `StreamEvents` bidirectional stream).

### 4.4 Cold Start and Readiness

**The EPP should NOT gate readiness on prefix cache sync.**

The current readiness check gates on `PoolHasSynced()` (whether the InferencePool CRD has been synced from the Kubernetes API) and leader status.

Reasons not to block readiness on prefix cache sync:

1. **The prefix cache scorer is one of several scorers.** The default scheduling profile includes `queue-scorer` (weight 2), `kv-cache-utilization-scorer` (weight 2), and `prefix-cache-scorer` (weight 3). A pod with an empty prefix cache still makes reasonable scheduling decisions using the other scorers.

2. **Deadlock risk.** If all 3 pods restart simultaneously (e.g., rolling update, node failure), no pod has state to sync FROM. If readiness is gated on sync, no pod ever becomes ready — a deadlock. The `Recreate` deployment strategy makes simultaneous restarts the common case.

3. **Capacity loss.** Blocking readiness means Envoy stops sending traffic to the new pod. In a 3-pod active-active setup, losing 1/3 of capacity while syncing is worse than serving with a cold prefix cache.

4. **The cache rebuilds naturally.** Each request processed by the new pod adds entries to the indexer via `PreRequest` → `indexer.Add()`. The cache warms up organically from traffic.

**Recommended startup sequence:**

```
Pod starts
  → Kubernetes API sync (PoolHasSynced)
  → Mark READY (Envoy starts sending traffic)
  → Simultaneously: discover peers via headless Service
  → Request SnapshotSync from one peer (background goroutine)
  → Apply snapshot to local indexer
  → Open StreamEvents to all peers
  → Continue normal operation
```

The brief window between becoming ready and completing the snapshot sync (sub-second for typical deployments) results in suboptimal prefix routing for a handful of requests — an acceptable tradeoff given that the other scorers still function correctly.

---

## 5. Summary

| Aspect | Decision | Rationale |
|--------|----------|-----------|
| CRDT | Not needed | Operations are approximately commutative; inter-replica divergence is negligible vs. inherent approximation error |
| Transport | gRPC bidirectional streaming | Already a core dependency; zero new dependencies |
| Batching | Buffer 50-100ms, send batched events | Reduces RPC overhead from 100K to ~1K-2K messages |
| Peer discovery | Headless Service + EndpointSlice watch | Kubernetes-native; uses existing controller-runtime infrastructure |
| Snapshot sync | `SnapshotSync` RPC on startup | ~18MB, sub-second transfer; applied to empty LRU (no conflicts) |
| Readiness gating | Do NOT block on prefix sync | Deadlock risk; other scorers still work; cache warms naturally |
| New dependencies | Zero | gRPC, controller-runtime, and protobuf are all existing dependencies |

