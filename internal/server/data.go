package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultDataTTLMinutes = 480
	MaxDataTTLMinutes     = 10080
	MaxDataKeyLen         = 40
	MaxDataValueSize      = 2 << 20
	maxDataStoreEntries   = 10000
	maxDataStoreBytes     = 64 << 20
)

type dataEntry struct {
	value     string
	expiresAt time.Time
}

// DataStore is the bounded in-memory store used by asynchronous results and
// the /data endpoint. It is deliberately concrete: there is one production
// implementation and tests can create it directly.
type DataStore struct {
	mu          sync.RWMutex
	entries     map[string]*dataEntry
	storedBytes int
}

func NewDataStore() *DataStore {
	store := &DataStore{entries: make(map[string]*dataEntry)}
	go store.cleanupLoop()
	return store
}

func (s *DataStore) cleanupLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.cleanup()
	}
}

func (s *DataStore) cleanup() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if now.After(entry.expiresAt) {
			s.deleteLocked(key, entry)
		}
	}
}

func (s *DataStore) Get(key string) (string, bool) {
	s.mu.RLock()
	entry, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		s.mu.Lock()
		if current, ok := s.entries[key]; ok && current == entry {
			s.deleteLocked(key, entry)
		}
		s.mu.Unlock()
		return "", false
	}
	return entry.value, true
}

func (s *DataStore) Put(key, value string, ttl time.Duration) bool {
	if len(value) > MaxDataValueSize {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for existingKey, entry := range s.entries {
		if now.After(entry.expiresAt) {
			s.deleteLocked(existingKey, entry)
		}
	}
	if old, ok := s.entries[key]; ok {
		s.deleteLocked(key, old)
	}
	valueBytes := len(value)
	for len(s.entries) >= maxDataStoreEntries || s.storedBytes+valueBytes > maxDataStoreBytes {
		oldestKey, oldest := s.oldestLocked()
		if oldest == nil {
			return false
		}
		s.deleteLocked(oldestKey, oldest)
	}
	s.entries[key] = &dataEntry{value: value, expiresAt: now.Add(ttl)}
	s.storedBytes += valueBytes
	return true
}

// Expiry returns the current expiry time for a key. It is useful to callers
// that need to report storage retention without exposing the store map.
func (s *DataStore) Expiry(key string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[key]
	if !ok {
		return time.Time{}, false
	}
	return entry.expiresAt, true
}

func (s *DataStore) deleteLocked(key string, entry *dataEntry) {
	if current, ok := s.entries[key]; ok && current == entry {
		delete(s.entries, key)
		s.storedBytes -= len(entry.value)
		if s.storedBytes < 0 {
			s.storedBytes = 0
		}
	}
}

func (s *DataStore) oldestLocked() (string, *dataEntry) {
	var oldestKey string
	var oldest *dataEntry
	for key, entry := range s.entries {
		if oldest == nil || entry.expiresAt.Before(oldest.expiresAt) {
			oldestKey, oldest = key, entry
		}
	}
	return oldestKey, oldest
}

// NewDataHandler returns the handler for the durable-data compatibility
// endpoint. It does not know anything about model execution or delivery mode.
func NewDataHandler(store *DataStore) http.Handler {
	if store == nil {
		store = NewDataStore()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			dataHandleGet(w, r, store)
		case http.MethodPut:
			dataHandlePut(w, r, store)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

func dataHandleGet(w http.ResponseWriter, r *http.Request, store *DataStore) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "key query parameter is required")
		return
	}
	if len(key) > MaxDataKeyLen {
		writeError(w, http.StatusBadRequest, "key exceeds maximum length of "+strconv.Itoa(MaxDataKeyLen))
		return
	}
	value, ok := store.Get(key)
	if !ok {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(value))
}

func dataHandlePut(w http.ResponseWriter, r *http.Request, store *DataStore) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "key query parameter is required")
		return
	}
	if len(key) > MaxDataKeyLen {
		writeError(w, http.StatusBadRequest, "key exceeds maximum length of "+strconv.Itoa(MaxDataKeyLen))
		return
	}
	ttlMinutes := DefaultDataTTLMinutes
	if ttlString := r.URL.Query().Get("ttl"); ttlString != "" {
		value, err := strconv.Atoi(ttlString)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ttl value")
			return
		}
		if value < 0 {
			writeError(w, http.StatusBadRequest, "ttl must not be negative")
			return
		}
		if value > 0 {
			ttlMinutes = value
		}
	}
	if ttlMinutes > MaxDataTTLMinutes {
		ttlMinutes = MaxDataTTLMinutes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(MaxDataValueSize+1)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) > MaxDataValueSize {
		writeError(w, http.StatusRequestEntityTooLarge, "body exceeds maximum size of 2MB")
		return
	}
	if !store.Put(key, string(body), time.Duration(ttlMinutes)*time.Minute) {
		writeError(w, http.StatusInsufficientStorage, "data store capacity exhausted")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "key": key, "ttl": ttlMinutes})
}
