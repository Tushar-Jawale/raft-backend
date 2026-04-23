package api

import (
	"fmt"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"net/http"
	"os"
	"path/filepath"
	"raft-go-backend/internal/inmem"
	"raft-go-backend/internal/raftcore"
	"sort"
	"strings"
	"time"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // allow all CORS for ws
	},
}

type KvItem struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Key     string `json:"key"`
	Field   string `json:"field"`
	Value   string `json:"value"`
}

func SetupRouter(wsManager *WebSocketManager, client *inmem.KVClient, clusterConfig map[string]inmem.NodeConfig) *gin.Engine {
	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowAllOrigins: true,
		AllowMethods:    []string{"*"},
		AllowHeaders:    []string{"*"},
	}))

	r.GET("/ws", func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			fmt.Printf("WS Upgrade error: %v\n", err)
			return
		}
		wsManager.AddClient(conn)

		go func() {
			defer func() {
				wsManager.RemoveClient(conn)
				conn.Close()
			}()
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					break // client disconnected
				}
			}
		}()
	})

	r.POST("/kv-store", func(c *gin.Context) {
		var item KvItem
		if err := c.BindJSON(&item); err != nil {
			c.JSON(400, gin.H{"success": false, "error": "Invalid request"})
			return
		}

		timestamp := time.Now().UnixMicro()
		res, err := client.Set(item.Key, item.Field, item.Value, timestamp, nil)
		if err != nil {
			c.JSON(500, gin.H{"success": false, "error": "INTERNAL_ERROR", "message": err.Error()})
			return
		}

		if res.Success {
			// 202 Accepted: the entry has been appended to the leader's log and
			// replication to followers is in progress. It will be committed once
			// a majority of nodes confirm. Watch WebSocket 'entries_committed'
			// events for durable confirmation.
			c.JSON(202, gin.H{
				"success":   true,
				"message":   "KV store entry appended to leader log — replication in progress",
				"key":       item.Key,
				"field":     item.Field,
				"value":     item.Value,
				"timestamp": timestamp,
				"log_index": res.Index,
				"term":      res.Term,
				"status":    "pending_commit",
			})
		} else if res.Error == "NOT_LEADER" || res.Error == "NO_LEADER" {
			c.JSON(503, gin.H{"success": false, "error": res.Error, "message": "Election in progress — please retry shortly."})
		} else {
			c.JSON(500, gin.H{"success": false, "error": res.Error, "message": res.Message})
		}
	})

	r.GET("/kv-store", func(c *gin.Context) {
		key := c.Query("key")
		field := c.Query("field")
		timestamp := time.Now().UnixMicro()

		res, err := client.Get(key, field, timestamp, "")
		if err != nil {
			c.JSON(500, gin.H{"success": false, "error": "INTERNAL_ERROR", "message": err.Error()})
			return
		}

		c.JSON(200, gin.H{
			"success": true,
			"message": "Data retrieved successfully",
			"value":   res.Value,
		})
	})

	r.DELETE("/kv-store", func(c *gin.Context) {
		key := c.Query("key")
		field := c.Query("field")
		timestamp := time.Now().UnixMicro()

		// Step 1: Check if key/field exists before deleting (matches Python behavior)
		checkRes, err := client.Get(key, field, timestamp, "")
		if err != nil {
			c.JSON(503, gin.H{"success": false, "error": "CONNECTION_ERROR", "message": err.Error()})
			return
		}
		if !checkRes.Success {
			c.JSON(404, gin.H{
				"success": false,
				"error":   "DATA_NOT_FOUND",
				"message": fmt.Sprintf("The provided data '%s.%s' does not exist in the database.", key, field),
			})
			return
		}

		// Step 2: Proceed with deletion
		res, err := client.Delete(key, field, timestamp)
		if err != nil {
			c.JSON(500, gin.H{"success": false, "error": "INTERNAL_ERROR", "message": err.Error()})
			return
		}

		c.JSON(200, gin.H{
			"success": true,
			"key":     key,
			"field":   field,
			"result":  "Entry appended to leader's log for deletion",
			"details": res,
		})
	})

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
			"websocket": gin.H{
				"connected_clients":    wsManager.GetClientCount(),
				"event_loop_registered": true,
			},
			"cluster": gin.H{
				"healthy": true,
				"config":  clusterConfig,
			},
		})
	})

	r.GET("/cluster/config", func(c *gin.Context) {
		c.JSON(200, clusterConfig)
	})

	r.GET("/cluster/status", func(c *gin.Context) {
		// Build node name list matching Python: "A, B, C, D, E"
		nodeNames := make([]string, 0, len(clusterConfig))
		for name := range clusterConfig {
			nodeNames = append(nodeNames, name)
		}
		sort.Strings(nodeNames)
		nodeList := strings.Join(nodeNames, ", ")

		c.JSON(200, gin.H{
			"nodes":       clusterConfig,
			"node_count":  len(clusterConfig),
			"description": fmt.Sprintf("RAFT cluster with %d nodes (%s)", len(clusterConfig), nodeList),
		})
	})

	r.POST("/cluster/node/:node_id/toggle", func(c *gin.Context) {
		nodeID := c.Param("node_id")
		conn, err := client.GetConnection(nodeID)
		if err != nil {
			c.JSON(500, gin.H{"success": false, "message": "Failed to connect to node"})
			return
		}
		
		var statusRes raftcore.NodeStatusResponse 
		err = conn.Call("RaftService.TogglePower", struct{}{}, &statusRes)
		
		if err != nil {
			c.JSON(500, gin.H{"success": false, "message": err.Error()})
			return
		}
		
		c.JSON(200, statusRes)
	})

	// ─── Serve frontend static files (production build) ───
	// Looks for the built React app in ../raft-frontend/dist
	frontendDist := filepath.Join("..", "raft-frontend", "dist")
	if info, err := os.Stat(frontendDist); err == nil && info.IsDir() {
		fmt.Println("[Cluster] Serving frontend from", frontendDist)
		r.Static("/assets", filepath.Join(frontendDist, "assets"))
		r.StaticFile("/vite.svg", filepath.Join(frontendDist, "vite.svg"))

		// SPA fallback: serve index.html for any unmatched route
		r.NoRoute(func(c *gin.Context) {
			c.File(filepath.Join(frontendDist, "index.html"))
		})
	} else {
		fmt.Println("[Cluster] No frontend build found at", frontendDist, "— run 'npm run build' in raft-frontend/")
	}

	return r
}

