package raftcore

import (
	"fmt"
	"math/rand"
	"net"
	"net/rpc"
	"sync"
	"time"
)

const rpcDialTimeout = 2 * time.Second

func dialWithTimeout(network, address string) (*rpc.Client, error) {
	conn, err := net.DialTimeout(network, address, rpcDialTimeout)
	if err != nil {
		return nil, err
	}
	return rpc.NewClient(conn), nil
}

type StateMachineApplier interface {
	Apply(commandStr string) map[string]interface{}
}

type WebSocketBroadcaster interface {
	BroadcastHeartbeat(leaderID string, currentTerm int, lastLogIndex int, lastLogTerm int, followers []string, peerResponses map[string]interface{})
	BroadcastPeerResponse(leaderID, peerID string, success bool, result interface{})
	BroadcastVoteRequest(nodeID string, term, lastLogIndex, lastLogTerm int)
	BroadcastVoteResponse(nodeID string, votedFor string, term, lastLogIndex, lastLogTerm int)
	BroadcastElectionResult(nodeID string, won bool, votedBy []string, term, lastLogIndex, lastLogTerm int)
	BroadcastLogEntry(nodeID string, command string, index int, committed bool)
	BroadcastEntriesCommitted(nodeID string, commitIndex, term int)
	BroadcastKVStoreUpdate(nodeID string, index int, logEntry LogEntry, result map[string]interface{})
	// BroadcastNodePowerChange notifies all clients when a node is killed or revived
	// so every open browser tab stays in sync with the actual cluster state.
	BroadcastNodePowerChange(nodeID string, status string)
}

type Raft struct {
	ID                  string
	Peers               map[string]struct{ Host string; Port int }
	LogsFilePath        string
	StateMachineApplier StateMachineApplier
	WSManager           WebSocketBroadcaster

	mu          sync.Mutex
	State       RaftState
	CurrentTerm int
	VotedFor    string
	CommitIndex int
	LastApplied int
	LeaderID    string
	Logs        []LogEntry

	NextIndex  map[string]int
	MatchIndex map[string]int

	Killed bool

	heartbeatStop chan struct{}
	electionReset chan struct{}

	voteCount     int
	receivedVotes map[string]bool
}

func NewRaft(id string, peers map[string]struct{ Host string; Port int }, applier StateMachineApplier, wsManager WebSocketBroadcaster) *Raft {
	r := &Raft{
		ID:                  id,
		Peers:               peers,
		LogsFilePath:        fmt.Sprintf("raft_node_%s.jsonl", id),
		StateMachineApplier: applier,
		WSManager:           wsManager,
		State:               Follower,
		CurrentTerm:         0,
		VotedFor:            "-1",
		CommitIndex:         -1,
		LastApplied:         -1,
		LeaderID:            "",
		Logs:                []LogEntry{},
		NextIndex:           make(map[string]int),
		MatchIndex:          make(map[string]int),
		electionReset:       make(chan struct{}, 1),
	}

	for peerID := range peers {
		r.NextIndex[peerID] = 0
		r.MatchIndex[peerID] = -1
	}

	r.loadPersistentState()
	r.recoverStateMachine()

	return r
}

func (r *Raft) loadPersistentState() {
	state, err := LoadPersistentState(r.ID)
	if err == nil {
		r.CurrentTerm = state.CurrentTerm
		r.VotedFor = state.VotedFor
		r.CommitIndex = state.CommitIndex
		r.LastApplied = state.LastApplied
	}

	logs, _ := LoadLogs(r.ID)
	r.Logs = logs
}

func (r *Raft) recoverStateMachine() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := 0; i <= r.CommitIndex; i++ {
		if i >= len(r.Logs) {
			break
		}
		if r.StateMachineApplier != nil {
			r.StateMachineApplier.Apply(r.Logs[i].Command)
			r.LastApplied = i
		}
	}
}

func (r *Raft) Start() {
	r.mu.Lock()
	r.heartbeatStop = make(chan struct{})
	r.mu.Unlock()
	go r.ticker()
	go r.heartbeatTicker()
}

func (r *Raft) Kill() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Killed {
		return
	}
	r.Killed = true
	r.State = Follower
	r.VotedFor = "-1"
	r.LeaderID = ""
	if r.heartbeatStop != nil {
		close(r.heartbeatStop)
	}
	select {
	case r.electionReset <- struct{}{}:
	default:
	}
}

func (r *Raft) Revive() {
	r.mu.Lock()
	if !r.Killed {
		r.mu.Unlock()
		return
	}
	r.Killed = false
	r.State = Follower
	r.VotedFor = "-1"
	r.voteCount = 0
	r.receivedVotes = make(map[string]bool)
	r.electionReset = make(chan struct{}, 1) // Fresh channel to avoid stale signals
	r.mu.Unlock()

	r.Start()
}

func (r *Raft) IsKilled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Killed
}

func (r *Raft) persistState() {
	state := PersistentState{
		NodeID:      r.ID,
		CurrentTerm: r.CurrentTerm,
		VotedFor:    r.VotedFor,
		CommitIndex: r.CommitIndex,
		LastApplied: r.LastApplied,
	}
	SavePersistentState(r.ID, state)
}

func (r *Raft) resetElectionTimer() {
	select {
	case r.electionReset <- struct{}{}:
	default:
	}
}

func (r *Raft) ticker() {
	for {
		if r.IsKilled() {
			return
		}

		timeout := time.Duration(rand.Intn(5000)+5000) * time.Millisecond
		fmt.Printf("[Node %s] Ticker waiting for up to %.3fs...\n", r.ID, timeout.Seconds())

		select {
		case <-r.electionReset:
			// Reset
		case <-time.After(timeout):
			r.mu.Lock()
			state := r.State
			r.mu.Unlock()
			if state != Leader && !r.IsKilled() {
				fmt.Printf("[Node %s] Election timeout! Starting election...\n", r.ID)
				r.startElection()
			}
		}
	}
}

func (r *Raft) startElection() {
	r.mu.Lock()
	r.CurrentTerm++
	r.State = Candidate
	r.VotedFor = r.ID
	r.LeaderID = ""
	r.voteCount = 1
	r.receivedVotes = map[string]bool{r.ID: true}
	fmt.Printf("[Node %s] Starting election for term %d\n", r.ID, r.CurrentTerm)
	r.persistState()

	lastLogIndex := len(r.Logs) - 1
	lastLogTerm := 0
	if lastLogIndex >= 0 {
		lastLogTerm = r.Logs[lastLogIndex].Term
	}

	args := VoteArguments{
		CandidateID:  r.ID,
		CurrentTerm:  r.CurrentTerm,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}

	peers := r.Peers
	currentTerm := r.CurrentTerm
	r.mu.Unlock()

	r.resetElectionTimer()

	if r.WSManager != nil {
		r.WSManager.BroadcastVoteRequest(r.ID, currentTerm, lastLogIndex, lastLogTerm)
		// Broadcast self-vote so the frontend counts the candidate's own vote
		r.WSManager.BroadcastVoteResponse(r.ID, r.ID, currentTerm, lastLogIndex, lastLogTerm)
	}

	var wg sync.WaitGroup
	for peerID, peerCfg := range peers {
		wg.Add(1)
		go func(pID string, host string, port int) {
			defer wg.Done()
			client, err := dialWithTimeout("tcp", fmt.Sprintf("%s:%d", host, port))
			if err != nil {
				return
			}
			defer client.Close()

			var reply VoteResponse
			err = client.Call("RaftService.RequestVote", args, &reply)
			if err == nil {
				r.mu.Lock()
				defer r.mu.Unlock()
				
				if r.State != Candidate || r.CurrentTerm != currentTerm {
					return
				}
				
				if reply.VoteGranted {
					r.voteCount++
					r.receivedVotes[pID] = true
				} else if reply.Term > r.CurrentTerm {
					r.CurrentTerm = reply.Term
					r.State = Follower
					r.VotedFor = "-1"
					r.persistState()
					go r.resetElectionTimer()
				}
			}
		}(peerID, peerCfg.Host, peerCfg.Port)
	}

	wg.Wait()

	r.mu.Lock()
	majority := (len(peers) + 1)/2 + 1
	won := r.voteCount >= majority && r.State == Candidate
	fmt.Printf("[Node %s] Election result: %d/%d votes (need %d)\n", r.ID, r.voteCount, len(peers)+1, majority)

	if won {
		fmt.Printf("[Node %s] WON ELECTION for term %d!\n", r.ID, currentTerm)
		r.State = Leader
		r.persistState()
		for pID := range r.Peers {
			r.NextIndex[pID] = len(r.Logs)
			r.MatchIndex[pID] = -1
		}
		r.MatchIndex[r.ID] = len(r.Logs) - 1
	}
	r.mu.Unlock()

	var votedBy []string
	r.mu.Lock()
	for p := range r.receivedVotes {
		votedBy = append(votedBy, p)
	}
	r.mu.Unlock()

	if r.WSManager != nil {
		r.WSManager.BroadcastElectionResult(r.ID, won, votedBy, currentTerm, lastLogIndex, lastLogTerm)
	}

	if won {
		fmt.Printf("[Node %s] Sending immediate heartbeat to establish leadership\n", r.ID)
		r.sendHeartbeats()
	}
}

func (r *Raft) heartbeatTicker() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		stopCh := r.heartbeatStop
		r.mu.Unlock()

		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()
			isLeader := r.State == Leader
			r.mu.Unlock()
			if isLeader && !r.IsKilled() {
				r.sendHeartbeats()
			}
		}
	}
}

func (r *Raft) sendHeartbeats() {
	r.mu.Lock()
	if r.State != Leader {
		r.mu.Unlock()
		return
	}
	
	lastLogIndex := len(r.Logs) - 1
	lastLogTerm := 0
	if lastLogIndex >= 0 {
		lastLogTerm = r.Logs[lastLogIndex].Term
	}
	
	var followers []string
	for pID := range r.Peers {
		followers = append(followers, pID)
	}
	peers := r.Peers
	currentTerm := r.CurrentTerm
	r.mu.Unlock()

	if r.WSManager != nil {
		r.WSManager.BroadcastHeartbeat(r.ID, currentTerm, lastLogIndex, lastLogTerm, followers, map[string]interface{}{})
	}

	// Fire-and-forget: match Python's ThreadPoolExecutor pattern
	// Don't block on wg.Wait() — peer RPCs complete asynchronously
	for peerID, peerCfg := range peers {
		go func(pID string, host string, port int) {
			r.sendHeartbeatToPeer(pID, host, port)
		}(peerID, peerCfg.Host, peerCfg.Port)
	}
}

func (r *Raft) sendHeartbeatToPeer(peerID string, host string, port int) {
	r.mu.Lock()
	if r.State != Leader {
		r.mu.Unlock()
		return
	}
	
	nextIdx := r.NextIndex[peerID]
	prevLogIndex := nextIdx - 1
	prevLogTerm := 0
	if prevLogIndex >= 0 && prevLogIndex < len(r.Logs) {
		prevLogTerm = r.Logs[prevLogIndex].Term
	}

	var entries []LogEntry
	if nextIdx < len(r.Logs) {
		entries = append(entries, r.Logs[nextIdx]) // sending one by one
	}

	args := HealthCheckArguments{
		CurrentTerm:  r.CurrentTerm,
		LeaderID:     r.ID,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: r.CommitIndex,
	}
	r.mu.Unlock()

	client, err := dialWithTimeout("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return
	}
	defer client.Close()

	var reply HealthCheckResponse
	err = client.Call("RaftService.HealthCheck", args, &reply)
	
	if r.WSManager != nil {
		success := err == nil && reply.Success
		res := map[string]interface{}{}
		if err == nil {
			res["term"] = reply.Term
			res["success"] = reply.Success
		} else {
			res["success"] = false
		}
		
		// Artificially delay peer_response broadcast to emulate Python's RPyC network latency (~100ms)
		// We ONLY broadcast peer_response if there are actual log entries replicated or a failure.
		// The original Python backend failed to broadcast on heartbeats due to an RPyC threading exception.
		// Sending peer_response during a heartbeat overwrites the frontend's activeMessage state,
		// killing the visual cyan heartbeat animation. Committing to this behavior restores UI parity.
		go func(leader, peer string, isSuccess bool, resp map[string]interface{}) {
			if len(entries) > 0 || !isSuccess {
				time.Sleep(100 * time.Millisecond)
				r.WSManager.BroadcastPeerResponse(leader, peer, isSuccess, resp)
			}
		}(r.ID, peerID, success, res)
	}

	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if reply.Term > r.CurrentTerm {
		r.CurrentTerm = reply.Term
		r.State = Follower
		r.VotedFor = "-1"
		return
	}

	if !reply.Success {
		if r.NextIndex[peerID] > 0 {
			r.NextIndex[peerID]--
		}
		return
	}

	if len(args.Entries) > 0 {
		newMatchIndex := args.PrevLogIndex + len(args.Entries)
		r.MatchIndex[peerID] = newMatchIndex
		r.NextIndex[peerID] = newMatchIndex + 1
		r.tryAdvanceCommitIndex()
	}
}

func (r *Raft) tryAdvanceCommitIndex() {
	for index := len(r.Logs) - 1; index > r.CommitIndex; index-- {
		replicatedCount := 1
		for pID := range r.Peers {
			if r.MatchIndex[pID] >= index {
				replicatedCount++
			}
		}

		majority := (len(r.Peers) + 1)/2 + 1
		if replicatedCount >= majority && r.Logs[index].Term == r.CurrentTerm {
			r.CommitIndex = index
			
			if r.WSManager != nil {
				r.WSManager.BroadcastEntriesCommitted(r.ID, r.CommitIndex, r.CurrentTerm)
			}
			
			r.applyCommittedEntries()
			r.persistState()
			break
		}
	}
}

func (r *Raft) applyCommittedEntries() {
	for r.LastApplied < r.CommitIndex {
		r.LastApplied++
		entry := r.Logs[r.LastApplied]

		var result map[string]interface{}
		if r.StateMachineApplier != nil {
			result = r.StateMachineApplier.Apply(entry.Command)
		}

		if r.WSManager != nil {
			r.WSManager.BroadcastKVStoreUpdate(r.ID, r.LastApplied, entry, result)
		}
	}
}

// Handlers for RPC

func (r *Raft) HandleRequestVote(args VoteArguments) VoteResponse {
	if r.IsKilled() {
		fmt.Printf("[Node %s] KILLED - ignoring RequestVote from Node %s\n", r.ID, args.CandidateID)
		return VoteResponse{Term: 0, VoteGranted: false}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Printf("[Node %s] Received RequestVote from Node %s (term %d)\n", r.ID, args.CandidateID, args.CurrentTerm)

	if args.CurrentTerm < r.CurrentTerm {
		return VoteResponse{Term: r.CurrentTerm, VoteGranted: false}
	}

	if args.CurrentTerm > r.CurrentTerm {
		r.CurrentTerm = args.CurrentTerm
		r.State = Follower
		r.LeaderID = ""
		r.VotedFor = "-1"
	}

	canVote := r.VotedFor == "-1" || r.VotedFor == args.CandidateID

	receiverLastTerm := 0
	receiverLastIndex := len(r.Logs) - 1
	if receiverLastIndex >= 0 {
		receiverLastTerm = r.Logs[receiverLastIndex].Term
	}

	logIsCurrent := false
	if args.LastLogTerm != receiverLastTerm {
		logIsCurrent = args.LastLogTerm > receiverLastTerm
	} else {
		logIsCurrent = args.LastLogIndex >= receiverLastIndex
	}

	if canVote && logIsCurrent {
		r.VotedFor = args.CandidateID
		r.persistState()
		go r.resetElectionTimer()
		
		fmt.Printf("[Node %s] GRANTED vote to Node %s for term %d\n", r.ID, args.CandidateID, args.CurrentTerm)
		if r.WSManager != nil {
			r.WSManager.BroadcastVoteResponse(r.ID, args.CandidateID, r.CurrentTerm, args.LastLogIndex, args.LastLogTerm)
		}
		
		return VoteResponse{Term: r.CurrentTerm, VoteGranted: true}
	}

	fmt.Printf("[Node %s] REJECTED vote from Node %s...\n", r.ID, args.CandidateID)
	if r.WSManager != nil {
		r.WSManager.BroadcastVoteResponse(r.ID, "-1", r.CurrentTerm, args.LastLogIndex, args.LastLogTerm)
	}
	return VoteResponse{Term: r.CurrentTerm, VoteGranted: false}
}

func (r *Raft) HandleHealthCheck(args HealthCheckArguments) HealthCheckResponse {
	if r.IsKilled() {
		fmt.Printf("[Node %s] KILLED - ignoring AppendEntries from Node %s\n", r.ID, args.LeaderID)
		return HealthCheckResponse{Term: 0, Success: false}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	
	if len(args.Entries) > 0 {
		fmt.Printf("[Node %s] Received AppendEntries from Node %s (term %d)\n", r.ID, args.LeaderID, args.CurrentTerm)
	}

	if args.CurrentTerm < r.CurrentTerm {
		return HealthCheckResponse{Term: r.CurrentTerm, Success: false}
	}

	if args.CurrentTerm >= r.CurrentTerm {
		r.CurrentTerm = args.CurrentTerm
		r.State = Follower
		r.LeaderID = args.LeaderID
		r.persistState()
		go r.resetElectionTimer()
	}

	if args.PrevLogIndex >= 0 {
		if args.PrevLogIndex >= len(r.Logs) {
			return HealthCheckResponse{Term: r.CurrentTerm, Success: false}
		}
		if r.Logs[args.PrevLogIndex].Term != args.PrevLogTerm {
			r.Logs = r.Logs[:args.PrevLogIndex]
			TruncateLogs(r.ID, args.PrevLogIndex)
			return HealthCheckResponse{Term: r.CurrentTerm, Success: false}
		}
	}

	if len(args.Entries) > 0 {
		insertIdx := args.PrevLogIndex + 1
		r.Logs = r.Logs[:insertIdx]
		TruncateLogs(r.ID, insertIdx)

		for i, entry := range args.Entries {
			r.Logs = append(r.Logs, entry)
			logIdx := args.PrevLogIndex + i + 1
			AppendLogEntry(r.ID, logIdx, entry.Term, entry.Command)
			
			if r.WSManager != nil {
				r.WSManager.BroadcastLogEntry(r.ID, entry.Command, logIdx, false)
			}
		}
	}

	if args.LeaderCommit > r.CommitIndex {
		r.CommitIndex = args.LeaderCommit
		if r.CommitIndex > len(r.Logs)-1 {
			r.CommitIndex = len(r.Logs) - 1
		}
		r.persistState()
		
		if r.WSManager != nil {
			r.WSManager.BroadcastEntriesCommitted(r.ID, r.CommitIndex, r.CurrentTerm)
		}
		
		r.applyCommittedEntries()
	}

	return HealthCheckResponse{Term: r.CurrentTerm, Success: true}
}

func (r *Raft) AppendCommand(command string) CommandResult {
	r.mu.Lock()
	
	if r.State != Leader {
		hint := r.getLeaderIDLocked()
		r.mu.Unlock()
		return CommandResult{
			Success:    false,
			Error:      "NOT_LEADER",
			LeaderHint: hint,
		}
	}

	entry := LogEntry{Term: r.CurrentTerm, Command: command}
	r.Logs = append(r.Logs, entry)
	logIndex := len(r.Logs) - 1
	currentTerm := r.CurrentTerm
	r.MatchIndex[r.ID] = logIndex
	r.NextIndex[r.ID] = logIndex + 1
	
	AppendLogEntry(r.ID, logIndex, entry.Term, entry.Command)
	r.mu.Unlock()

	if r.WSManager != nil {
		r.WSManager.BroadcastLogEntry(r.ID, command, logIndex, false)
	}

	return CommandResult{
		Success: true,
		Index:   logIndex,
		Term:    currentTerm,
	}
}

func (r *Raft) ReadLog(index int) ReadLogResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	if index < 0 || index >= len(r.Logs) {
		return ReadLogResult{Success: false, Error: "INDEX_OUT_OF_RANGE"}
	}

	isCommitted := index <= r.CommitIndex
	if r.State != Leader && !isCommitted {
		return ReadLogResult{Success: false, Error: "NOT_COMMITTED"}
	}

	return ReadLogResult{
		Success:   true,
		Entry:     r.Logs[index],
		Committed: isCommitted,
		Index:     index,
	}
}

// getLeaderIDLocked returns leader ID; caller MUST hold r.mu
func (r *Raft) getLeaderIDLocked() string {
	if r.State == Leader {
		return r.ID
	}
	return r.LeaderID
}

// GetLeaderID is the thread-safe public version
func (r *Raft) GetLeaderID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getLeaderIDLocked()
}

func (r *Raft) GetRaftData() ([]LogEntry, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Logs, r.CommitIndex
}
