package app

import (
	"capelin-go/internal/server"
	"net/http"
	"time"
)

type dataStore struct{ store *server.DataStore }

func newDataStore() *dataStore { return &dataStore{store: server.NewDataStore()} }

func (s *dataStore) serverStore() *server.DataStore {
	if s == nil {
		return server.NewDataStore()
	}
	if s.store == nil {
		s.store = server.NewDataStore()
	}
	return s.store
}

func (s *dataStore) Get(key string) (string, bool) { return s.serverStore().Get(key) }
func (s *dataStore) Put(key, value string, ttl time.Duration) bool {
	return s.serverStore().Put(key, value, ttl)
}
func (s *dataStore) Expiry(key string) (time.Time, bool) { return s.serverStore().Expiry(key) }

func (a *app) dataHandler(w http.ResponseWriter, r *http.Request) {
	server.NewDataHandler(a.dataStore.serverStore()).ServeHTTP(w, r)
}
