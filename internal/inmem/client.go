package inmem

import (
	"errors"
	"fmt"
	"net/rpc"
	"raft-go-backend/internal/raftcore"
	"sync"
	"time"
)

type NodeConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type KVClient struct {
	ClusterConfig map[string]NodeConfig
	mu            sync.Mutex
	leaderID      string
	connections   map[string]*rpc.Client
	leaderCond    *sync.Cond              // signaled when a leader is elected
	pendingKeys   map[string]chan struct{} // per-key channels: closed when the write is committed
}

func NewKVClient(clusterConfig map[string]NodeConfig) *KVClient {
	c := &KVClient{
		ClusterConfig: clusterConfig,
		leaderID:      "",
		connections:   make(map[string]*rpc.Client),
		pendingKeys:   make(map[string]chan struct{}),
	}
	c.leaderCond = sync.NewCond(&c.mu)
	return c
}

func (c *KVClient) GetConnection(nodeID string) (*rpc.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getConnectionLocked(nodeID)
}

func (c *KVClient) getConnectionLocked(nodeID string) (*rpc.Client, error) {
	conn, ok := c.connections[nodeID]
	if ok && conn != nil {
		return conn, nil
	}
	cfg, ok := c.ClusterConfig[nodeID]
	if !ok {
		return nil, fmt.Errorf("unknown node: %s", nodeID)
	}
	client, err := rpc.Dial("tcp", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port))
	if err != nil {
		c.connections[nodeID] = nil
		return nil, err
	}
	c.connections[nodeID] = client
	return client, nil
}

// NotifyLeaderElected is called by the WebSocketManager when a node wins election.
func (c *KVClient) NotifyLeaderElected(leaderID string) {
	c.mu.Lock()
	c.leaderID = leaderID
	c.mu.Unlock()
	c.leaderCond.Broadcast()
	fmt.Printf("[KVClient] Leader %s elected — waking all waiting requests\n", leaderID)
}

// DiscoverLeader polls nodes to find the current leader.
func (c *KVClient) DiscoverLeader() string {
	for nodeID := range c.ClusterConfig {
		c.mu.Lock()
		conn, err := c.getConnectionLocked(nodeID)
		c.mu.Unlock()
		if err != nil {
			continue
		}
		var info raftcore.LeaderInfo
		err = conn.Call("RaftService.GetLeader", struct{}{}, &info)
		if err != nil {
			c.mu.Lock()
			c.connections[nodeID] = nil
			c.mu.Unlock()
			continue
		}
		if info.State == string(raftcore.Leader) {
			c.mu.Lock()
			c.leaderID = info.NodeID
			c.mu.Unlock()
			return info.NodeID
		}
		if info.LeaderID != "" {
			c.mu.Lock()
			c.leaderID = info.LeaderID
			c.mu.Unlock()
			return info.LeaderID
		}
	}
	return ""
}

// SendToLeader sends a command to the leader. If no leader exists, the goroutine
// blocks until one is elected (via NotifyLeaderElected) or 15s timeout.
func (c *KVClient) SendToLeader(commandStr string) (raftcore.CommandResult, error) {
	timedOut := false
	timer := time.AfterFunc(15*time.Second, func() {
		c.mu.Lock()
		timedOut = true
		c.leaderCond.Broadcast()
		c.mu.Unlock()
	})
	defer timer.Stop()

	for {
		c.mu.Lock()
		for c.leaderID == "" && !timedOut {
			fmt.Println("[KVClient] No leader — request blocked, waiting for election...")
			c.leaderCond.Wait()
		}
		if c.leaderID == "" {
			c.mu.Unlock()
			return raftcore.CommandResult{
				Success: false, Error: "NO_LEADER",
				Message: "Election timed out — no leader available.",
			}, errors.New("no leader after 15s")
		}
		leader := c.leaderID
		conn, err := c.getConnectionLocked(leader)
		c.mu.Unlock()

		if err != nil {
			c.mu.Lock()
			if c.leaderID == leader {
				c.leaderID = ""
			}
			c.mu.Unlock()
			continue
		}

		var result raftcore.CommandResult
		rpcErr := conn.Call("RaftService.AppendLogEntries", raftcore.AppendLogArgs{Command: commandStr}, &result)
		if rpcErr != nil {
			c.mu.Lock()
			c.connections[leader] = nil
			if c.leaderID == leader {
				c.leaderID = ""
			}
			c.mu.Unlock()
			continue
		}

		if result.Success {
			return result, nil
		}
		if result.Error == "NOT_LEADER" {
			c.mu.Lock()
			if result.LeaderHint != "" {
				c.leaderID = result.LeaderHint
				c.leaderCond.Broadcast()
			} else if c.leaderID == leader {
				c.leaderID = ""
			}
			c.mu.Unlock()
			continue
		}
		return result, nil
	}
}

// Set registers a per-key barrier, then sends the write to the leader.
// Any GET for the same key will block until this SET is committed (or fails).
func (c *KVClient) Set(key, field, value string, timestamp int64, ttl *int64) (raftcore.CommandResult, error) {
	cacheKey := key + "." + field

	// Register a pending-write barrier for this key
	doneCh := make(chan struct{})
	c.mu.Lock()
	c.pendingKeys[cacheKey] = doneCh
	c.mu.Unlock()

	fmt.Printf("[KVClient] SET %s — registered pending write barrier\n", cacheKey)

	// When SendToLeader returns (committed or failed), close the barrier to unblock readers
	defer func() {
		c.mu.Lock()
		if c.pendingKeys[cacheKey] == doneCh {
			delete(c.pendingKeys, cacheKey)
		}
		c.mu.Unlock()
		close(doneCh)
		fmt.Printf("[KVClient] SET %s — write barrier released\n", cacheKey)
	}()

	return c.SendToLeader(CreateSetCommand(key, field, value, timestamp, ttl))
}

// Delete registers a per-key barrier, then sends the delete to the leader.
func (c *KVClient) Delete(key, field string, timestamp int64) (raftcore.CommandResult, error) {
	cacheKey := key + "." + field

	doneCh := make(chan struct{})
	c.mu.Lock()
	c.pendingKeys[cacheKey] = doneCh
	c.mu.Unlock()

	fmt.Printf("[KVClient] DELETE %s — registered pending write barrier\n", cacheKey)

	defer func() {
		c.mu.Lock()
		if c.pendingKeys[cacheKey] == doneCh {
			delete(c.pendingKeys, cacheKey)
		}
		c.mu.Unlock()
		close(doneCh)
		fmt.Printf("[KVClient] DELETE %s — write barrier released\n", cacheKey)
	}()

	return c.SendToLeader(CreateDeleteCommand(key, field, timestamp))
}

type ReadKVArgs struct {
	Key       string
	Field     string
	Timestamp int64
}

type ReadKVResponse struct {
	Success bool
	Value   string
}

// Get reads a key. If that key has a pending uncommitted write (SET or DELETE),
// the GET blocks until the write is committed, then reads the fresh value from the cluster.
// Reads for other keys proceed instantly from any available node.
func (c *KVClient) Get(key, field string, timestamp int64, targetNode string) (ReadKVResponse, error) {
	cacheKey := key + "." + field

	// Check if this key has a pending write
	c.mu.Lock()
	doneCh, hasPending := c.pendingKeys[cacheKey]
	c.mu.Unlock()

	if hasPending {
		fmt.Printf("[KVClient] GET %s — blocked waiting for pending write to commit...\n", cacheKey)
		select {
		case <-doneCh:
			// Write committed (or failed) — read fresh value from cluster
			fmt.Printf("[KVClient] GET %s — pending write completed, reading fresh value\n", cacheKey)
		case <-time.After(15 * time.Second):
			return ReadKVResponse{}, fmt.Errorf("timeout waiting for pending write on key %s", cacheKey)
		}
	}

	// Normal read from any available node
	for attempt := 0; attempt < 3; attempt++ {
		node := targetNode
		if node == "" {
			c.mu.Lock()
			node = c.leaderID
			c.mu.Unlock()
			if node == "" {
				for n := range c.ClusterConfig {
					node = n
					break
				}
			}
		}
		conn, err := c.GetConnection(node)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		var res ReadKVResponse
		err = conn.Call("RaftService.ReadKV", ReadKVArgs{Key: key, Field: field, Timestamp: timestamp}, &res)
		if err != nil {
			c.mu.Lock()
			c.connections[node] = nil
			c.mu.Unlock()
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return res, nil
	}
	return ReadKVResponse{}, fmt.Errorf("could not reach any node for GET")
}

func (c *KVClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, conn := range c.connections {
		if conn != nil {
			conn.Close()
		}
		c.connections[k] = nil
	}
}
