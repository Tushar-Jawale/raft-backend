package inmem

type StateMachineApplier struct {
	db *ByteDataDB
}

func NewStateMachineApplier(db *ByteDataDB) *StateMachineApplier {
	return &StateMachineApplier{db: db}
}

func (sma *StateMachineApplier) Apply(commandStr string) map[string]interface{} {
	cmd, err := ConvertCommandToKVCommand(commandStr)
	if err != nil {
		return map[string]interface{}{"success": false, "error": "Invalid command format"}
	}

	if cmd.Type == CommandTypeSet {
		sma.db.SetAt(cmd.Key, cmd.Field, cmd.Value, cmd.Timestamp, cmd.TTL)
		return map[string]interface{}{
			"success":   true,
			"operation": "SET",
			"key":       cmd.Key,
			"field":     cmd.Field,
			"value":     cmd.Value,
			"timestamp": cmd.Timestamp,
		}
	} else if cmd.Type == CommandTypeDelete {
		deleted := sma.db.DeleteAt(cmd.Key, cmd.Field, cmd.Timestamp)
		return map[string]interface{}{
			"success":   true,
			"operation": "DELETE",
			"key":       cmd.Key,
			"deleted":   deleted,
		}
	}

	return map[string]interface{}{"success": false, "error": "Unknown command type"}
}

func (sma *StateMachineApplier) GetState() map[string]interface{} {
	return map[string]interface{}{
		"success": true,
		"data":    sma.db.GetEntireData(),
	}
}
