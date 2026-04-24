# Raft Consensus Backend

> A production-quality Go implementation of the **Raft consensus algorithm** powering a distributed, fault-tolerant key-value store with real-time cluster visualization via WebSocket.

This backend runs a five-node Raft cluster entirely within a single Go process, exposes a REST + WebSocket API for a React frontend, and persists node state to disk so the cluster recovers correctly after a restart. Every internal Raft event (vote request, heartbeat, log append, commit) is broadcast to connected WebSocket clients in real time.

---

## Table of Contents

- [Project Overview](#project-overview)
- [Key Features](#key-features)
- [Tech Stack](#tech-stack)
- [System Architecture](#system-architecture)
- [How It Works](#how-it-works)
- [API Routes Reference](#api-routes-reference)
- [WebSocket Events](#websocket-events)
- [Installation & Setup](#installation--setup)
- [Environment Variables](#environment-variables)
- [Folder Structure](#folder-structure)
- [Response Format](#response-format)
- [Key Design Decisions](#key-design-decisions)
- [Future Enhancements](#future-enhancements)

---

## Project Overview

Raft Consensus Backend is a **backend-first** distributed systems project built to demonstrate the internals of the Raft consensus algorithm with full observability. It implements leader election, log replication, and commit propagation across a five-node in-process cluster, all visualizable through a companion React dashboard.

**Core workflow:**

```
Boot Cluster --> Leader Election --> Accept KV Writes --> Replicate to Followers
    --> Commit on Majority --> Apply to State Machine --> Broadcast via WebSocket
```

---

## Key Features

- **Full Raft Consensus Implementation:** Complete leader election, log replication, and commit with strict majority quorum. The randomized election timeout (5-10 s) is intentionally wide for frontend observability.
- **Five-Node In-Process Cluster:** All nodes run as goroutines in a single binary communicating over loopback TCP via Go's `net/rpc`. No external coordination or service discovery required.
- **Fault Injection & Recovery:** Kill and revive individual nodes via the REST API to observe re-election, log catch-up, and cluster self-healing in real time.
- **Real-Time Observability:** Every Raft event (votes, heartbeats, log appends, commits, power changes) is broadcast over WebSocket, enabling a frontend to animate the entire cluster lifecycle.
- **Distributed Key-Value Store:** A thread-safe in-memory KV store (`key.field -> value`) backed by the replicated Raft log, with read-after-write consistency guarantees.
- **Crash Recovery & Persistence:** Each node persists its log (JSONL) and durable state (JSON) to disk. On restart, logs are replayed through the state machine to restore pre-crash state.
- **Docker-Ready:** Multi-stage Dockerfile produces a minimal Alpine image for containerized deployment.

---

## Tech Stack

| Component | Technology |
|---|---|
| **Language** | Go 1.25 |
| **Web Framework** | [Gin](https://github.com/gin-gonic/gin) v1.12 |
| **WebSocket** | [Gorilla WebSocket](https://github.com/gorilla/websocket) v1.5 |
| **Inter-Node RPC** | Go standard library `net/rpc` over TCP |
| **CORS** | [gin-contrib/cors](https://github.com/gin-contrib/cors) v1.7 |
| **Persistence** | JSONL (append-only log) + JSON (durable state) on disk |
| **Containerization** | Docker (multi-stage Alpine build) |
| **Config** | `os.LookupEnv` (no `.env` loader at runtime) |

---

## System Architecture

The application is built as a single-process, multi-goroutine cluster backed by Go's `net/rpc` for inter-node communication and a Gin HTTP server for external API and WebSocket access.

```
+-----------------------------------------+
|        React Frontend (Client)          |
|     WebSocket  .  HTTP REST             |
+--------+---------------------+----------+
| REST /kv-store |      | ws://host/ws |
         v                     v
+-----------------+   +----------------------+
| Gin HTTP Router |   |  WebSocket Manager   |
| GET/POST/DELETE |   |  broadcast to all    |
| /kv-store       |   |  connected clients   |
| /health         |   +----------+-----------+
| /cluster/*      |              | events
+--------+--------+              |
         | write/read            | leader elected
         v                       |
+-------------------------+      |
|  KV Client              |------+
|  leader-aware           |
|  write-barrier          |
|  read-after-write       |
+--------+----------------+
         | net/rpc AppendLogEntries / ReadKV
         v
+----------------------------------------------------------------+
|  Raft Cluster (5 nodes, in-process)                            |
|                                                                |
|  +--------+  +--------+  +--------+  +--------+  +--------+    |
|  | Node A |  | Node B |  | Node C |  | Node D |  | Node E |    |
|  | :5001  |<>| :5002  |<>| :5003  |<>| :5004  |<>| :5005  |    |
|  |(Leader)|  |Follower|  |Follower|  |Follower|  |Follower|    |
|  +--------+  +--------+  +--------+  +--------+  +--------+    |
|         Go net/rpc over TCP - AppendEntries . RequestVote      |
+----------------------------+-----------------------------------+
                        | persist |
            +----------------+-------------------+
            v                                    v
  raft_node_{id}.jsonl               raft_node_{id}_state.json
  (append-only log)                  (term, votedFor, commitIndex)
```

### Key Architectural Decisions

- **Single-Process Cluster** -- All five nodes run as goroutines in the same binary, communicating over loopback TCP using Go's `net/rpc`. Self-contained and easy to run without external dependencies.
- **Real-Time Event Broadcasting** -- Every Raft state change is broadcast via WebSocket, enabling full cluster visualization without polling.
- **Write-Barrier Read Consistency** -- The KV Client blocks `GET` requests for keys with in-flight writes, preventing read-your-writes anomalies.
- **Deterministic Recovery** -- Persistent JSONL logs and JSON state files enable crash recovery by replaying committed entries through the state machine on startup.
- **Fire-and-Forget Heartbeats** -- Heartbeat RPCs are dispatched as goroutines without blocking on responses, keeping the heartbeat interval stable regardless of slow peers.
- **One-Entry-at-a-Time Replication** -- The leader sends at most one log entry per replication call, simplifying implementation and making replication clearly visible in the frontend.

---

## How It Works

### Leader Election

Each node starts as a **Follower** with an election timeout sampled uniformly at random from 5-10 seconds. When the timeout fires without receiving a heartbeat from a leader, the node increments its term, transitions to **Candidate**, votes for itself, and broadcasts `RequestVote` RPCs to all peers. A peer grants its vote if it has not already voted in this term and the candidate's log is at least as up-to-date. If the candidate collects votes from a strict majority (3 of 5), it becomes **Leader** and immediately sends heartbeats to suppress competing elections.

### Log Replication

Once elected, the leader accepts write commands via `AppendLogEntries` RPC, appends each entry to its own log, persists it to disk, and sends `HealthCheck` (AppendEntries) RPCs to each follower with `prevLogIndex`/`prevLogTerm` for consistency checking. Followers that detect a mismatch reject the entry, causing the leader to decrement `nextIndex[peer]` and retry. Heartbeats are sent every 2 seconds using the same RPC path with empty entries.

### Commit & State Machine

An entry is committed once replicated to a majority. The leader advances `commitIndex` and piggybacks this on subsequent heartbeats. When committed, the state machine applies the command:

```
KVCommand (JSON) --> StateMachineApplier.Apply() --> ByteDataDB.SetAt() / DeleteAt()
```

Each applied entry triggers a `kv_store_update` WebSocket broadcast for real-time client updates.

### Read-After-Write Consistency

The KV Client implements a write-barrier pattern. Before forwarding a `SET` or `DELETE` to the leader, it registers a pending channel keyed by `key.field`. Any concurrent `GET` for the same key blocks on this channel until the write completes. This ensures a `POST` followed immediately by `GET` always returns the written value.

### Node Power Toggle

`POST /cluster/node/:node_id/toggle` kills or revives a node via RPC. Killing sets the node to Follower, clears its leader hint, and stops all goroutines. Reviving restarts election and heartbeat tickers. Both transitions broadcast a `node_power_change` WebSocket event.

---

## API Routes Reference

*Base URL: `http://localhost:8765`*

### Key-Value Store

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/kv-store` | Write a key-field-value triplet to the cluster via the leader |
| `GET` | `/kv-store?key=&field=` | Read a value (blocks if a write is pending for the same key) |
| `DELETE` | `/kv-store?key=&field=` | Delete a field (validates existence, then queues deletion) |

### Cluster Management

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/health` | Server health check with WebSocket client count and cluster status |
| `GET` | `/cluster/config` | Full cluster node configuration map |
| `GET` | `/cluster/status` | Human-readable cluster summary (node count, node IDs) |
| `POST` | `/cluster/node/:node_id/toggle` | Kill or revive a specific node (A, B, C, D, or E) |

### WebSocket

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/ws` | Upgrade to WebSocket; receives full cluster state on connect |

---

## WebSocket Events

All events are JSON objects with a `type` field and an ISO 8601 `timestamp`. Subscribe at `ws://host:8765/ws`.

| Event Type | Trigger | Key Fields |
|---|---|---|
| `heartbeat` | Every 2s by the current leader | `leader_id`, `current_term`, `last_log_index`, `followers` |
| `peer_response` | Leader receives replication response from a follower | `leader_id`, `peer_id`, `success`, `term` |
| `vote_request` | Candidate begins soliciting votes | `node_id`, `current_term`, `last_log_index` |
| `vote_response` | Node casts or refuses a vote | `node_id`, `voted_for`, `current_term` |
| `election_result` | Candidate completes election round | `node_id`, `election_result`, `voted_by`, `current_term` |
| `log_entry` | New entry appended to any node's log | `node_id`, `log_entry`, `log_index`, `committed` |
| `entries_committed` | Node advances its `commitIndex` | `node_id`, `committed_until_index`, `current_term` |
| `kv_store_update` | Committed entry applied to the state machine | `node_id`, `key`, `field`, `value`, `log_index` |
| `node_power_change` | Node is killed or revived | `node_id`, `status` (`dead` / `alive`) |

---

## Installation & Setup

### Prerequisites

- Go 1.25+
- (Optional) Docker for containerized deployment

### 1. Clone the repository

```bash
git clone https://github.com/Tushar-Jawale/raft-go-backend.git
cd raft-go-backend
```

### 2. Install dependencies

```bash
go mod download
```

### 3. Start the cluster

```bash
go run ./cmd/server/main.go
```

The API will be available at `http://localhost:8765`. Within 5-10 seconds, a leader will be elected and the cluster will begin accepting writes.

### 4. Test the API

```bash
# Write a value
curl -s -X POST http://localhost:8765/kv-store \
  -H 'Content-Type: application/json' \
  -d '{"key":"user:1","field":"name","value":"Alice"}' | jq .

# Read it back
curl -s "http://localhost:8765/kv-store?key=user:1&field=name" | jq .

# Kill a node
curl -s -X POST http://localhost:8765/cluster/node/B/toggle | jq .

# Check cluster health
curl -s http://localhost:8765/health | jq .
```

### Docker

```bash
# Build the image
docker build -t raft-go-backend .

# Run with default settings
docker run -p 8765:8765 raft-go-backend

# Override the HTTP port
docker run -p 9000:9000 -e PORT=9000 raft-go-backend
```

> **Note:** When running in Docker, `RAFT_INTERNAL_HOST` defaults to `127.0.0.1`, which is correct for single-container deployments where all nodes communicate on the loopback interface.

---

## Environment Variables

Environment variables are read directly via `os.LookupEnv`. There is no automatic `.env` file loading at runtime -- export variables in your shell, use your IDE's run config, or pass them as Docker `-e` flags.

| Variable | Default | Description |
|---|---|---|
| `HOST` | `0.0.0.0` | HTTP/WS server bind address |
| `PORT` | `8765` | HTTP/WS server port |
| `RAFT_INTERNAL_HOST` | `127.0.0.1` | Host address for internal Raft RPC listeners |

The five Raft nodes always bind to `RAFT_INTERNAL_HOST` on ports `5001`-`5005`. These are not individually configurable at runtime.

---

## Folder Structure

```text
raft-go-backend/
├── cmd/
│   └── server/
│       └── main.go              # Entry point: boots nodes, RPC servers, HTTP/WS
│
├── internal/
│   ├── api/
│   │   ├── router.go            # Gin routes: /kv-store, /health, /cluster/*
│   │   └── websocket.go         # WebSocket manager + all broadcast methods
│   ├── inmem/
│   │   ├── db.go                # Thread-safe in-memory KV store (ByteDataDB)
│   │   ├── applier.go           # State machine: applies SET/DELETE commands
│   │   ├── command.go           # KVCommand JSON marshalling (SET, DELETE)
│   │   └── client.go            # KVClient: leader discovery, write-barrier, GET
│   └── raftcore/
│       ├── raft.go              # Core Raft logic: election, heartbeat, replication
│       ├── rpc.go               # RaftService: net/rpc method handlers
│       ├── state.go             # Persistence: load/save logs and state files
│       └── types.go             # All shared types and RPC argument structs
│
├── Dockerfile                   # Multi-stage build (golang:alpine -> alpine)
├── go.mod                       # Project dependencies
├── go.sum
└── .env                         # Example environment variables
```

Each `raft_node_{id}.jsonl` (log) and `raft_node_{id}_state.json` (durable state) file is created at runtime in the working directory.

---

## Response Format

All endpoints return a consistent JSON structure:

```json
// Success
{
  "success": true,
  "message": "Data retrieved successfully",
  "value": "Alice"
}

// Error
{
  "success": false,
  "error": "NO_LEADER",
  "message": "Election in progress - please retry shortly."
}
```

HTTP status codes follow REST conventions: `200 OK`, `202 Accepted`, `404 Not Found`, `503 Service Unavailable`, `500 Internal Server Error`.

---

## Key Design Decisions

| Decision | Rationale |
|---|---|
| **Single-process cluster** | All five nodes as goroutines in one binary makes the system self-contained. Trades network partition simulation for ease of deployment. |
| **`HealthCheck` = `AppendEntries`** | A naming convention from the Python prototype. Serves double duty as heartbeat (empty entries) and replication message (non-empty entries). |
| **One-entry-at-a-time replication** | Simplifies implementation and makes replication clearly visible in the frontend at the cost of throughput for bulk writes. |
| **Fire-and-forget heartbeats** | Heartbeats dispatched as goroutines without waiting for all peers, keeping the interval stable regardless of slow peers. |
| **Peer-response suppression** | `peer_response` events are only broadcast during actual replication, preventing blank heartbeat ACKs from overwriting frontend animations. |
| **Write-barrier reads** | `GET` requests block for keys with in-flight `SET`/`DELETE`, preventing the classic read-your-writes anomaly. |

---

## Persistence & Recovery

Each Raft node persists two files to the working directory on every state change:

| File | Format | Contents |
|---|---|---|
| `raft_node_{id}.jsonl` | JSONL | Append-only log of `{index, term, command}` entries |
| `raft_node_{id}_state.json` | JSON | `currentTerm`, `votedFor`, `commitIndex`, `lastApplied` |

**Recovery sequence on startup:**

1. `loadPersistentState` reads the state JSON and restores term, vote, and commit progress.
2. `LoadLogs` scans the JSONL file and rebuilds the in-memory log slice.
3. `recoverStateMachine` replays all entries up to `commitIndex` through the state machine applier, restoring the KV store to its pre-crash state.

**Log truncation:** When a follower rejects an `AppendEntries` due to a log inconsistency, the leader decrements `nextIndex` and retries. The follower rewrites its `.jsonl` file atomically, keeping only the consistent prefix.

---

## Future Enhancements

- **Snapshot / Log Compaction** -- Prevents unbounded log growth by periodically snapshotting the state machine.
- **Configurable Cluster Size** -- Allow dynamic cluster size at startup instead of the hardcoded five-node configuration.
- **Membership Changes** -- Joint consensus protocol for adding/removing nodes from a running cluster.
- **Batched Log Replication** -- Send multiple entries per RPC for improved write throughput.
- **gRPC Transport** -- Replace `net/rpc` with gRPC for better cross-language interoperability and streaming support.
- **Network Partition Simulation** -- Separate listener per node process to simulate real network-level partitions.

---

