package inmem

import (
	"sort"
	"strings"
	"sync"
)

type ByteDataField struct {
	Value     string
	CreatedAt int64
	TTL       *int64
}

func (f *ByteDataField) IsAlive(timestamp int64) bool {
	if f.TTL == nil {
		return true
	}
	return timestamp <= f.CreatedAt+(*f.TTL*1000)
}

type ByteDataRecord struct {
	Fields map[string]*ByteDataField
}

type ByteDataDB struct {
	mu    sync.RWMutex
	store map[string]*ByteDataRecord
}

func NewByteDataDB() *ByteDataDB {
	return &ByteDataDB{
		store: make(map[string]*ByteDataRecord),
	}
}

var instance *ByteDataDB
var once sync.Once

func GetInstance() *ByteDataDB {
	once.Do(func() {
		instance = NewByteDataDB()
	})
	return instance
}

func (db *ByteDataDB) SetAt(key, field, value string, timestamp int64, ttl *int64) {
	db.mu.Lock()
	defer db.mu.Unlock()

	record, ok := db.store[key]
	if !ok {
		record = &ByteDataRecord{Fields: make(map[string]*ByteDataField)}
		db.store[key] = record
	}
	record.Fields[field] = &ByteDataField{
		Value:     value,
		CreatedAt: timestamp,
		TTL:       ttl,
	}
}

func (db *ByteDataDB) GetAt(key, field string, timestamp int64) *string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	record, ok := db.store[key]
	if !ok {
		return nil
	}

	f, ok := record.Fields[field]
	if ok && f.IsAlive(timestamp) {
		return &f.Value
	}
	return nil
}

func (db *ByteDataDB) DeleteAt(key, field string, timestamp int64) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	record, ok := db.store[key]
	if !ok {
		return false
	}

	f, ok := record.Fields[field]
	if ok && f.IsAlive(timestamp) {
		delete(record.Fields, field)
		if len(record.Fields) == 0 {
			delete(db.store, key)
		}
		return true
	}
	return false
}

func (db *ByteDataDB) GetEntireData() map[string]map[string]map[string]interface{} {
	db.mu.RLock()
	defer db.mu.RUnlock()
	res := make(map[string]map[string]map[string]interface{})
	for key, record := range db.store {
		res[key] = make(map[string]map[string]interface{})
		res[key]["fields"] = make(map[string]interface{})
		for fieldName, field := range record.Fields {
			res[key]["fields"][fieldName] = map[string]interface{}{
				"value": field.Value,
			}
		}
	}
	return res
}

func (db *ByteDataDB) ScanFields(key string, timestamp int64) [][2]string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	record, ok := db.store[key]
	if !ok {
		return nil
	}

	var result [][2]string
	for k, f := range record.Fields {
		if f.IsAlive(timestamp) {
			result = append(result, [2]string{k, f.Value})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i][0] < result[j][0]
	})

	return result
}

func (db *ByteDataDB) ScanFieldsByPrefix(key, prefix string, timestamp int64) [][2]string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	record, ok := db.store[key]
	if !ok {
		return nil
	}

	var result [][2]string
	for k, f := range record.Fields {
		if f.IsAlive(timestamp) && strings.HasPrefix(k, prefix) {
			result = append(result, [2]string{k, f.Value})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i][0] < result[j][0]
	})

	return result
}
