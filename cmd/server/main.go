package main

import (
	"fmt"
	"net"
	"net/rpc"
	"os"
	"os/signal"
	"raft-go-backend/internal/api"
	"raft-go-backend/internal/inmem"
	"raft-go-backend/internal/raftcore"
	"syscall"
	"time"
)

func getEnvOrDefault(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

var clusterConfigMap = map[string]inmem.NodeConfig{
	"A": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5001},
	"B": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5002},
	"C": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5003},
	"D": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5004},
	"E": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5005},
}

var raftConfigMap = map[string]struct{ Host string; Port int }{
	"A": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5001},
	"B": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5002},
	"C": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5003},
	"D": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5004},
	"E": {Host: getEnvOrDefault("RAFT_INTERNAL_HOST", "127.0.0.1"), Port: 5005},
}

func main() {
	fmt.Println("============================================================")
	fmt.Println("RAFT Cluster Go Backend")
	fmt.Println("============================================================")

	wsManager := api.NewWebSocketManager()
	nodes := make(map[string]*raftcore.Raft)

	fmt.Println("Phase 1: Initialize RPC Servers")
	
	for nodeID, cfg := range clusterConfigMap {
		fmt.Printf("[Cluster] Starting Node %s...\n", nodeID)

		db := inmem.NewByteDataDB()
		applier := inmem.NewStateMachineApplier(db)

		raftPeers := make(map[string]struct{ Host string; Port int })
		for pID, pCfg := range raftConfigMap {
			if pID != nodeID {
				raftPeers[pID] = pCfg
			}
		}

		raftNode := raftcore.NewRaft(nodeID, raftPeers, applier, wsManager)
		nodes[nodeID] = raftNode
		wsManager.RegisterNode(nodeID, raftNode, applier)

		rpcServer := rpc.NewServer()
		raftService := raftcore.NewRaftService(raftNode, db.GetAt)
		rpcServer.RegisterName("RaftService", raftService)

		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port))
		if err != nil {
			fmt.Printf("[Node %s] Error starting listener: %v\n", nodeID, err)
			os.Exit(1)
		}

		go func(l net.Listener, id string) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go rpcServer.ServeConn(conn)
			}
		}(listener, nodeID)

		fmt.Printf("[Node %s] RPC server listening on %s:%d\n", nodeID, cfg.Host, cfg.Port)
	}

	time.Sleep(1 * time.Second)
	fmt.Println("\n[Cluster] All RPC servers started. Waiting for stabilization...")

	fmt.Println("Phase 2: Begin Leader Election")
	for nodeID, node := range nodes {
		fmt.Printf("[Cluster] Starting election timer for Node %s\n", nodeID)
		node.Start()
	}

	kvClient := inmem.NewKVClient(clusterConfigMap)
	defer kvClient.Close()

	// Wire up election notifications so queued requests are processed when a leader wins
	wsManager.SetKVClient(kvClient)

	router := api.SetupRouter(wsManager, kvClient, clusterConfigMap)
	
	httpHost := getEnvOrDefault("HOST", "0.0.0.0")
	httpPort := getEnvOrDefault("PORT", "8765")
	httpAddr := fmt.Sprintf("%s:%s", httpHost, httpPort)

	go func() {
		fmt.Printf("[Cluster] HTTP/WS Server spinning up on %s\n", httpAddr)
		err := router.Run(httpAddr)
		if err != nil {
			fmt.Printf("Router error: %v\n", err)
			os.Exit(1)
		}
	}()

	fmt.Printf("\nConnect your React app to: ws://%s/ws\n", httpAddr)
	if httpHost == "0.0.0.0" {
		fmt.Printf("Note: If running locally, you can connect to ws://localhost:%s/ws\n", httpPort)
	}
	fmt.Print("\nThe cluster will continue running. Press Ctrl+C to stop.\n")

	waitForLeader(nodes)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n\n[Cluster] Shutting down...")
	for _, node := range nodes {
		node.Kill()
	}
	fmt.Println("[Cluster] ✓ All nodes stopped")
}

func waitForLeader(nodes map[string]*raftcore.Raft) {
	fmt.Println("\n[Cluster] Waiting up to 15 seconds for leader election...")
	timeout := time.After(15 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			fmt.Println("LEADER ELECTION FAILED THRU TIMEOUT")
			return
		case <-ticker.C:
			for nodeID, node := range nodes {
				if node.GetLeaderID() == nodeID {
					fmt.Println("LEADER ELECTION SUCCESSFUL!")
					fmt.Printf("Cluster is running with Node %s as leader.\n", nodeID)
					return
				}
			}
		}
	}
}
