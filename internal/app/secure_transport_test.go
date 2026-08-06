package app

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSecureHTTPClientReusesPersistentConnection(t *testing.T) {
	var newConnections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	client := NewSecureHTTPClient(&secureDialPolicy{
		AllowPrivate: func(string) bool { return true },
	}, func(u *url.URL) error {
		if u.String() != server.URL+"/" {
			return errors.New("unexpected target")
		}
		return nil
	}, 5)

	for i := 0; i < 2; i++ {
		resp, err := client.Get(server.URL + "/")
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if got := newConnections.Load(); got != 1 {
		t.Fatalf("new connections = %d, want one pooled connection", got)
	}
}

func TestSecureHTTPClientRejectsRedirectAndRecovers(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://blocked.example/private", http.StatusFound)
		case "/response-failure":
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		case "/ok":
			_, _ = io.WriteString(w, "recovered")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	allowedOrigin := strings.ToLower(server.URL)
	client := NewSecureHTTPClient(&secureDialPolicy{
		AllowPrivate: func(string) bool { return true },
	}, func(u *url.URL) error {
		if strings.ToLower(u.Scheme+"://"+u.Host) != allowedOrigin {
			return errors.New("target not allowed")
		}
		return nil
	}, 5)

	if _, err := client.Get(server.URL + "/redirect"); err == nil || !strings.Contains(err.Error(), "target not allowed") {
		t.Fatalf("redirect error = %v, want authorization failure", err)
	}
	if _, err := client.Get(server.URL + "/response-failure"); err == nil {
		t.Fatal("response failure request unexpectedly succeeded")
	}
	resp, err := client.Get(server.URL + "/ok")
	if err != nil {
		t.Fatalf("recovery request: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || string(body) != "recovered" {
		t.Fatalf("recovery response = %q, read error = %v", body, readErr)
	}
	if requests.Load() < 3 {
		t.Fatalf("server requests = %d, want redirect, response failure, and recovery", requests.Load())
	}
}
