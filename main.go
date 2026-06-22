package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

type CapturedRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Query   map[string][]string `json:"query"`
	Body    string              `json:"body"`
}

type Session struct {
	CreatedAt time.Time
	C         chan CapturedRequest
}

var (
	// In-memory runtime storage
	sessions = make(map[string]*Session)
	mu       sync.RWMutex

	// Rate Limiting Storage
	ipLimitMap = make(map[string]time.Time)
	ipMu       sync.Mutex

	// Constraints
	maxBodySize    int64 = 100 * 1024 // 100 KB max body to prevent OOM
	maxSessions          = 1000       // Max active links system-wide
	sessionTimeout       = 10 * time.Minute
	rateLimitDelay       = 2 * time.Second // Time between API generations per IP
)

// rateLimit checks if an IP is requesting too fast
func rateLimit(r *http.Request) error {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}

	ipMu.Lock()
	defer ipMu.Unlock()

	lastSeen, exists := ipLimitMap[ip]
	if exists && time.Since(lastSeen) < rateLimitDelay {
		return fmt.Errorf("rate limit exceeded, try again in a few seconds")
	}
	ipLimitMap[ip] = time.Now()
	return nil
}

func generateID() string {
	b := make([]byte, 4) // 8 character hex string (like amk33)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	// Background cleanup routine for memory management
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			mu.Lock()
			now := time.Now()
			for id, sess := range sessions {
				if now.Sub(sess.CreatedAt) > sessionTimeout {
					close(sess.C)
					delete(sessions, id)
				}
			}
			mu.Unlock()

			// Cleanup IP limits map to prevent memory leak
			ipMu.Lock()
			for ip, lastSeen := range ipLimitMap {
				if now.Sub(lastSeen) > 5*time.Minute {
					delete(ipLimitMap, ip)
				}
			}
			ipMu.Unlock()
		}
	}()

	http.HandleFunc("/", handler)

	port := ":7272"
	log.Printf("Starting HTTP Reverser on %s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}

func handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	// Serve UI
	case path == "/":
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)

	// Generate new ID
	case path == "/api/generate" && r.Method == http.MethodPost:
		if err := rateLimit(r); err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}

		mu.Lock()
		if len(sessions) >= maxSessions {
			mu.Unlock()
			http.Error(w, "Server capacity reached. Try again later.", http.StatusServiceUnavailable)
			return
		}
		id := generateID()
		sessions[id] = &Session{
			CreatedAt: time.Now(),
			C:         make(chan CapturedRequest, 1),
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id})

	// Server-Sent Events stream
	case strings.HasPrefix(path, "/api/stream/"):
		id := strings.TrimPrefix(path, "/api/stream/")

		mu.RLock()
		sess, exists := sessions[id]
		mu.RUnlock()

		if !exists {
			http.Error(w, "Session expired or invalid", http.StatusNotFound)
			return
		}

		// SSE Headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Wait for a request to come in
		select {
		case reqData, ok := <-sess.C:
			if !ok {
				return // Channel was closed (timeout)
			}
			data, _ := json.Marshal(reqData)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			// Client closed the connection
			return
		}

	// Capture incoming requests (The Reverser functionality)
	default:
		id := strings.TrimPrefix(path, "/")

		mu.Lock()
		sess, exists := sessions[id]
		if exists {
			// Auto-delete on first use to free memory and enforce single-use
			delete(sessions, id)
		}
		mu.Unlock()

		if !exists {
			http.Error(w, "Link not found, expired, or already used.", http.StatusNotFound)
			return
		}

		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, maxBodySize))

		captured := CapturedRequest{
			Method:  r.Method,
			URL:     r.URL.String(),
			Headers: r.Header,
			Query:   r.URL.Query(),
			Body:    string(bodyBytes),
		}

		// Send to waiting SSE client
		sess.C <- captured
		close(sess.C)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Request captured successfully.\n"))
	}
}
