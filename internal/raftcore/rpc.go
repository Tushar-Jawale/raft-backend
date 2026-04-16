package raftcore

type RaftService struct {
	raft    *Raft
	readDB  func(key, field string, ts int64) *string
}

func NewRaftService(raft *Raft, readDB func(string, string, int64) *string) *RaftService {
	return &RaftService{raft: raft, readDB: readDB}
}

func (rs *RaftService) RequestVote(args VoteArguments, reply *VoteResponse) error {
	*reply = rs.raft.HandleRequestVote(args)
	return nil
}

func (rs *RaftService) HealthCheck(args HealthCheckArguments, reply *HealthCheckResponse) error {
	*reply = rs.raft.HandleHealthCheck(args)
	return nil
}

func (rs *RaftService) AppendLogEntries(args AppendLogArgs, reply *CommandResult) error {
	*reply = rs.raft.AppendCommand(args.Command)
	return nil
}

func (rs *RaftService) ReadLog(index int, reply *ReadLogResult) error {
	*reply = rs.raft.ReadLog(index)
	return nil
}

func (rs *RaftService) GetLeader(ignore struct{}, reply *LeaderInfo) error {
	rs.raft.mu.Lock()
	defer rs.raft.mu.Unlock()
	*reply = LeaderInfo{
		NodeID:   rs.raft.ID,
		State:    string(rs.raft.State),
		Term:     rs.raft.CurrentTerm,
		LeaderID: rs.raft.getLeaderIDLocked(),
	}
	return nil
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

func (rs *RaftService) ReadKV(args ReadKVArgs, reply *ReadKVResponse) error {
	val := rs.readDB(args.Key, args.Field, args.Timestamp)
	if val != nil {
		*reply = ReadKVResponse{Success: true, Value: *val}
	} else {
		*reply = ReadKVResponse{Success: false, Value: "NOT_FOUND"}
	}
	return nil
}

func (rs *RaftService) TogglePower(ignore struct{}, reply *NodeStatusResponse) error {
	if rs.raft.IsKilled() {
		rs.raft.Revive()
		*reply = NodeStatusResponse{Success: true, NodeID: rs.raft.ID, Status: "alive", Message: "Node is testing"}
	} else {
		rs.raft.Kill()
		*reply = NodeStatusResponse{Success: true, NodeID: rs.raft.ID, Status: "dead", Message: "Node killed"}
	}
	return nil
}
