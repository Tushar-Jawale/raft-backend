package raftcore

// RaftState represents the state of a Raft node
type RaftState string

const (
	Leader    RaftState = "Leader"
	Follower  RaftState = "Follower"
	Candidate RaftState = "Candidate"
)

// LogEntry stored in the Raft log
type LogEntry struct {
	Term    int    `json:"term"`
	Command string `json:"command"`
}

type LogEntryWithIndex struct {
	Index   int    `json:"index"`
	Term    int    `json:"term"`
	Command string `json:"command"`
}

// VoteArguments sent by candidates to request a vote
type VoteArguments struct {
	CandidateID  string `json:"candidate_id"`
	CurrentTerm  int    `json:"current_term"`
	LastLogIndex int    `json:"last_log_index"`
	LastLogTerm  int    `json:"last_log_term"`
}

// VoteResponse is returned by followers after receiving VoteArguments
type VoteResponse struct {
	Term        int  `json:"term"`
	VoteGranted bool `json:"vote_granted"`
}

// HealthCheckArguments sent by leaders for heartbeat and log replication
type HealthCheckArguments struct {
	CurrentTerm  int        `json:"current_term"`
	LeaderID     string     `json:"leader_id"`
	PrevLogIndex int        `json:"prev_log_index"`
	PrevLogTerm  int        `json:"prev_log_term"`
	Entries      []LogEntry `json:"entries"`
	LeaderCommit int        `json:"leader_commit"`
}

// HealthCheckResponse is returned by followers after receiving HealthCheckArguments
type HealthCheckResponse struct {
	Term    int  `json:"term"`
	Success bool `json:"success"`
}

type NodeStatusResponse struct {
	Success bool   `json:"success"`
	NodeID  string `json:"node_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// CommandResult represents the result of applying a command or an RPC invocation
type CommandResult struct {
	Success    bool   `json:"success"`
	Index      int    `json:"index,omitempty"`
	Term       int    `json:"term,omitempty"`
	Error      string `json:"error,omitempty"`
	LeaderHint string `json:"leader_hint,omitempty"`
	Message    string `json:"message,omitempty"`
}

type ReadLogResult struct {
	Success   bool     `json:"success"`
	Entry     LogEntry `json:"entry,omitempty"`
	Committed bool     `json:"committed"`
	Index     int      `json:"index"`
	Error     string   `json:"error,omitempty"`
	Message   string   `json:"message,omitempty"`
}

type LeaderInfo struct {
	NodeID   string `json:"node_id"`
	State    string `json:"state"`
	Term     int    `json:"term"`
	LeaderID string `json:"leader_id,omitempty"`
}

type AppendLogArgs struct {
	Command string
}
