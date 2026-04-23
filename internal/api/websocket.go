package api

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"raft-go-backend/internal/inmem"
	"raft-go-backend/internal/raftcore"
	"sync"
	"time"
)

// safeConn wraps a websocket.Conn with a write mutex
// gorilla/websocket is NOT safe for concurrent writes
type safeConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (sc *safeConn) WriteJSON(msg []byte) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.conn.WriteMessage(websocket.TextMessage, msg)
}

type WebSocketManager struct {
	mu       sync.Mutex
	clients  map[*safeConn]bool
	nodes    map[string]*raftcore.Raft
	appliers map[string]*inmem.StateMachineApplier
	kvClient *inmem.KVClient // notified on leader election to drain request queue
}

func NewWebSocketManager() *WebSocketManager {
	return &WebSocketManager{
		clients:  make(map[*safeConn]bool),
		nodes:    make(map[string]*raftcore.Raft),
		appliers: make(map[string]*inmem.StateMachineApplier),
	}
}

// SetKVClient registers the KV client so it can be notified when a leader is elected.
func (wm *WebSocketManager) SetKVClient(client *inmem.KVClient) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.kvClient = client
}

func (wm *WebSocketManager) RegisterNode(nodeID string, node *raftcore.Raft, applier *inmem.StateMachineApplier) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.nodes[nodeID] = node
	wm.appliers[nodeID] = applier
}

func (wm *WebSocketManager) AddClient(conn *websocket.Conn) {
	sc := &safeConn{conn: conn}
	wm.mu.Lock()
	wm.clients[sc] = true
	wm.mu.Unlock()

	// syncState must NOT hold wm.mu while calling into Raft (lock ordering)
	wm.syncState(sc)
}

func (wm *WebSocketManager) RemoveClient(conn *websocket.Conn) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	for sc := range wm.clients {
		if sc.conn == conn {
			delete(wm.clients, sc)
			return
		}
	}
}

// syncState sends initial state to a new client.
// IMPORTANT: Does NOT hold wm.mu while calling Raft methods (avoids deadlock).
func (wm *WebSocketManager) syncState(sc *safeConn) {
	// Snapshot node references under lock
	wm.mu.Lock()
	nodesCopy := make(map[string]*raftcore.Raft, len(wm.nodes))
	for k, v := range wm.nodes {
		nodesCopy[k] = v
	}
	appliersCopy := make(map[string]*inmem.StateMachineApplier, len(wm.appliers))
	for k, v := range wm.appliers {
		appliersCopy[k] = v
	}
	wm.mu.Unlock()

	for nodeID, raftNode := range nodesCopy {
		// Send current power status
		status := "alive"
		if raftNode.IsKilled() {
			status = "dead"
		}
		wm.sendToClient(sc, map[string]interface{}{
			"type":      "node_power_change",
			"node_id":   nodeID,
			"status":    status,
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		})

		// GetRaftData acquires raft.mu internally — safe because we don't hold wm.mu
		logs, commitIndex := raftNode.GetRaftData()
		for i, logEntry := range logs {
			isCommitted := i <= commitIndex
			msg := map[string]interface{}{
				"type":      "log_entry",
				"node_id":   nodeID,
				"log_entry": logEntry.Command,
				"log_index": i,
				"committed": isCommitted,
				"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			}
			wm.sendToClient(sc, msg)
		}

		if applier, ok := appliersCopy[nodeID]; ok && applier != nil {
			state := applier.GetState()
			if data, ok := state["data"].(map[string]map[string]map[string]interface{}); ok {
				for key, record := range data {
					if fields, ok := record["fields"]; ok {
						for fieldName, fieldObj := range fields {
							val := fieldObj.(map[string]interface{})["value"]
							msg := map[string]interface{}{
								"type":      "kv_store_update",
								"node_id":   nodeID,
								"log_index": -1,
								"log_entry": map[string]interface{}{"command": fmt.Sprintf("SET %s.%s=%v", key, fieldName, val)},
								"key":       key,
								"field":     fieldName,
								"value":     val,
								"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
							}
							wm.sendToClient(sc, msg)
						}
					}
				}
			}
		}
	}
}

func (wm *WebSocketManager) sendToClient(sc *safeConn, msg map[string]interface{}) {
	data, err := json.Marshal(msg)
	if err == nil {
		sc.WriteJSON(data)
	}
}

func (wm *WebSocketManager) broadcast(msg map[string]interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	wm.mu.Lock()
	clientsCopy := make([]*safeConn, 0, len(wm.clients))
	for sc := range wm.clients {
		clientsCopy = append(clientsCopy, sc)
	}
	wm.mu.Unlock()

	var toRemove []*safeConn
	for _, sc := range clientsCopy {
		err := sc.WriteJSON(data)
		if err != nil {
			sc.conn.Close()
			toRemove = append(toRemove, sc)
		}
	}

	if len(toRemove) > 0 {
		wm.mu.Lock()
		for _, sc := range toRemove {
			delete(wm.clients, sc)
		}
		wm.mu.Unlock()
	}
}

func (wm *WebSocketManager) BroadcastHeartbeat(leaderID string, currentTerm int, lastLogIndex int, lastLogTerm int, followers []string, peerResp map[string]interface{}) {
	msg := map[string]interface{}{
		"type":           "heartbeat",
		"leader_id":      leaderID,
		"current_term":   currentTerm,
		"last_log_index": lastLogIndex,
		"last_log_term":  lastLogTerm,
		"followers":      followers,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		"responses":      peerResp,
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) BroadcastPeerResponse(leaderID, peerID string, success bool, result interface{}) {
	var term interface{} = nil
	if resMap, ok := result.(map[string]interface{}); ok {
		if s, ok := resMap["success"].(bool); ok && s {
			term = resMap["term"]
		}
	}
	msg := map[string]interface{}{
		"type":      "peer_response",
		"leader_id": leaderID,
		"peer_id":   peerID,
		"success":   success,
		"term":      term,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) BroadcastVoteRequest(nodeID string, term, lastLogIndex, lastLogTerm int) {
	msg := map[string]interface{}{
		"type":           "vote_request",
		"node_id":        nodeID,
		"current_term":   term,
		"last_log_index": lastLogIndex,
		"last_log_term":  lastLogTerm,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) BroadcastVoteResponse(nodeID string, votedFor string, term, lastLogIndex, lastLogTerm int) {
	msg := map[string]interface{}{
		"type":           "vote_response",
		"node_id":        nodeID,
		"voted_for":      votedFor,
		"current_term":   term,
		"last_log_index": lastLogIndex,
		"last_log_term":  lastLogTerm,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) BroadcastElectionResult(nodeID string, won bool, votedBy []string, term, lastLogIndex, lastLogTerm int) {
	msg := map[string]interface{}{
		"type":            "election_result",
		"node_id":         nodeID,
		"election_result": won,
		"voted_by":        votedBy,
		"current_term":    term,
		"last_log_index":  lastLogIndex,
		"last_log_term":   lastLogTerm,
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)

	// Notify KVClient so queued requests are processed immediately
	if won {
		wm.mu.Lock()
		client := wm.kvClient
		wm.mu.Unlock()
		if client != nil {
			client.NotifyLeaderElected(nodeID)
		}
	}
}

func (wm *WebSocketManager) BroadcastLogEntry(nodeID string, command string, index int, committed bool) {
	msg := map[string]interface{}{
		"type":      "log_entry",
		"node_id":   nodeID,
		"log_entry": command,
		"log_index": index,
		"committed": committed,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) BroadcastEntriesCommitted(nodeID string, commitIndex, term int) {
	msg := map[string]interface{}{
		"type":                  "entries_committed",
		"node_id":               nodeID,
		"committed_until_index": commitIndex,
		"current_term":          term,
		"timestamp":             time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) BroadcastKVStoreUpdate(nodeID string, index int, logEntry raftcore.LogEntry, result map[string]interface{}) {
	msg := map[string]interface{}{
		"type":      "kv_store_update",
		"node_id":   nodeID,
		"log_index": index,
		"log_entry": logEntry,
		"key":       result["key"],
		"field":     result["field"],
		"value":     result["value"],
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) BroadcastNodePowerChange(nodeID string, status string) {
	msg := map[string]interface{}{
		"type":      "node_power_change",
		"node_id":   nodeID,
		"status":    status, // "alive" or "dead"
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	wm.broadcast(msg)
}

func (wm *WebSocketManager) GetClientCount() int {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return len(wm.clients)
}

// GetRawConn returns the underlying websocket conn for a safeConn - used by router for removal
func (wm *WebSocketManager) GetConnForRemoval(conn *websocket.Conn) {
	wm.RemoveClient(conn)
}
