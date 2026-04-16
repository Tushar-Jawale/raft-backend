package raftcore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type PersistentState struct {
	NodeID      string `json:"node_id"`
	CurrentTerm int    `json:"current_term"`
	VotedFor    string `json:"voted_for"`
	CommitIndex int    `json:"commit_index"`
	LastApplied int    `json:"last_applied"`
}

func SavePersistentState(nodeID string, state PersistentState) error {
	filename := fmt.Sprintf("raft_node_%s_state.json", nodeID)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0644)
}

func LoadPersistentState(nodeID string) (PersistentState, error) {
	filename := fmt.Sprintf("raft_node_%s_state.json", nodeID)
	state := PersistentState{
		NodeID:      nodeID,
		CurrentTerm: 0,
		VotedFor:    "-1",
		CommitIndex: -1,
		LastApplied: -1,
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil // Default state
		}
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func AppendLogEntry(nodeID string, index int, term int, command string) error {
	filename := fmt.Sprintf("raft_node_%s.jsonl", nodeID)
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	entry := LogEntryWithIndex{
		Index:   index,
		Term:    term,
		Command: command,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	_, err = f.WriteString(string(data) + "\n")
	return err
}

func LoadLogs(nodeID string) ([]LogEntry, error) {
	filename := fmt.Sprintf("raft_node_%s.jsonl", nodeID)
	logs := []LogEntry{}

	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return logs, nil
		}
		return logs, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry LogEntryWithIndex
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			fmt.Printf("[Node %s] Skipping corrupted log entry: %v\n", nodeID, err)
			continue
		}
		logs = append(logs, LogEntry{Term: entry.Term, Command: entry.Command})
	}
	return logs, scanner.Err()
}

func TruncateLogs(nodeID string, keepUpToIndex int) error {
	filename := fmt.Sprintf("raft_node_%s.jsonl", nodeID)
	
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	
	lines := []string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	f.Close()

	if keepUpToIndex > len(lines) {
		keepUpToIndex = len(lines)
	}

	keepLines := lines[:keepUpToIndex]

	fWrite, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer fWrite.Close()

	for _, line := range keepLines {
		if _, err := fWrite.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return nil
}
