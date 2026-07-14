package main

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
	defaultDataTTLMinutes = 480     // 8 hours
	maxDataTTLMinutes     = 10080   // 7 days
	maxDataKeyLen         = 40      // max key length in characters
	maxDataValueSize      = 2 << 20 // 2 MB max value size
	maxDataStoreEntries   = 10000
	maxDataStoreBytes     = 64 << 20 // aggregate in-memory value limit
)

type dataEntry struct {
	value     string
	expiresAt time.Time
}

type dataStore struct {
	mu          sync.RWMutex
	entries     map[string]*dataEntry
	storedBytes int
}

func newDataStore() *dataStore {
	s := &dataStore{entries: make(map[string]*dataEntry)}
	go s.cleanupLoop()
	return s
}

func (s *dataStore) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.cleanup()
	}
}

func (s *dataStore) cleanup() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if now.After(entry.expiresAt) {
			s.deleteLocked(key, entry)
		}
	}
}

func (s *dataStore) Get(key string) (string, bool) {
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

func (s *dataStore) Put(key, value string, ttl time.Duration) bool {
	if len(value) > maxDataValueSize {
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
	s.entries[key] = &dataEntry{
		value:     value,
		expiresAt: now.Add(ttl),
	}
	s.storedBytes += valueBytes
	return true
}

func (s *dataStore) deleteLocked(key string, entry *dataEntry) {
	if current, ok := s.entries[key]; ok && current == entry {
		delete(s.entries, key)
		s.storedBytes -= len(entry.value)
		if s.storedBytes < 0 {
			s.storedBytes = 0
		}
	}
}

func (s *dataStore) oldestLocked() (string, *dataEntry) {
	var oldestKey string
	var oldest *dataEntry
	for key, entry := range s.entries {
		if oldest == nil || entry.expiresAt.Before(oldest.expiresAt) {
			oldestKey, oldest = key, entry
		}
	}
	return oldestKey, oldest
}

func (a *app) dataHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.dataHandleGet(w, r)
	case http.MethodPut:
		a.dataHandlePut(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *app) dataHandleGet(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "key query parameter is required")
		return
	}
	if len(key) > maxDataKeyLen {
		writeError(w, http.StatusBadRequest, "key exceeds maximum length of "+strconv.Itoa(maxDataKeyLen))
		return
	}

	value, ok := a.dataStore.Get(key)
	if !ok {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(value))
}

func (a *app) dataHandlePut(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "key query parameter is required")
		return
	}
	if len(key) > maxDataKeyLen {
		writeError(w, http.StatusBadRequest, "key exceeds maximum length of "+strconv.Itoa(maxDataKeyLen))
		return
	}

	// Parse TTL.
	ttlMinutes := defaultDataTTLMinutes
	if ttlStr := r.URL.Query().Get("ttl"); ttlStr != "" {
		v, err := strconv.Atoi(ttlStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ttl value")
			return
		}
		if v < 0 {
			writeError(w, http.StatusBadRequest, "ttl must not be negative")
			return
		}
		if v > 0 {
			ttlMinutes = v
		}
	}
	if ttlMinutes > maxDataTTLMinutes {
		ttlMinutes = maxDataTTLMinutes
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxDataValueSize+1)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) > maxDataValueSize {
		writeError(w, http.StatusRequestEntityTooLarge, "body exceeds maximum size of 2MB")
		return
	}

	if !a.dataStore.Put(key, string(body), time.Duration(ttlMinutes)*time.Minute) {
		writeError(w, http.StatusInsufficientStorage, "data store capacity exhausted")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"ok":  true,
		"key": key,
		"ttl": ttlMinutes,
	})
}
