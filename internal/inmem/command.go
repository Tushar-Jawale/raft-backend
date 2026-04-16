package inmem

import "encoding/json"

type CommandType string

const (
	CommandTypeSet    CommandType = "SET"
	CommandTypeDelete CommandType = "DELETE"
)

type KVCommand struct {
	Type      CommandType `json:"type"`
	Key       string      `json:"key"`
	Field     string      `json:"field"`
	Value     string      `json:"value,omitempty"`
	Timestamp int64       `json:"timestamp"`
	TTL       *int64      `json:"ttl,omitempty"`
}

func CreateSetCommand(key, field, value string, timestamp int64, ttl *int64) string {
	cmd := KVCommand{
		Type:      CommandTypeSet,
		Key:       key,
		Field:     field,
		Value:     value,
		Timestamp: timestamp,
		TTL:       ttl,
	}
	data, _ := json.Marshal(cmd)
	return string(data)
}

func CreateDeleteCommand(key, field string, timestamp int64) string {
	cmd := KVCommand{
		Type:      CommandTypeDelete,
		Key:       key,
		Field:     field,
		Timestamp: timestamp,
	}
	data, _ := json.Marshal(cmd)
	return string(data)
}

func ConvertCommandToKVCommand(commandStr string) (KVCommand, error) {
	var cmd KVCommand
	err := json.Unmarshal([]byte(commandStr), &cmd)
	return cmd, err
}
