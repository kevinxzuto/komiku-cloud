package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"komiku-cloud/internal/store"
)

const maxBlobSize = 64 << 20 // 64 MiB, generous headroom over a typical backup

type Server struct {
	st       *store.Store
	limiters sync.Map // ip -> *rate.Limiter
}

func New(st *store.Store) *Server {
	return &Server{st: st}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sync/content", s.rateLimited(s.handleGetContent))
	mux.HandleFunc("PUT /api/sync/content", s.rateLimited(s.handlePutContent))
	mux.HandleFunc("POST /api/sync/event", s.rateLimited(s.handlePostEvent))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return logRequests(mux)
}

// logRequests gives visibility into what clients are actually doing, since
// this server otherwise has no access logging at all.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		keyPreview := safeKeyPreview(r.Header.Get("X-API-Token"))
		log.Printf(
			"%s %s ip=%s key=%s... status=%d if-match=%q if-none-match=%q",
			r.Method, r.URL.Path, clientIP(r), keyPreview, rw.status,
			r.Header.Get("If-Match"), r.Header.Get("If-None-Match"),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// rateLimited applies a simple per-IP token bucket before auth, so a bad
// actor without a valid key still can't hammer the DB.
func (s *Server) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		limiterAny, _ := s.limiters.LoadOrStore(ip, rate.NewLimiter(rate.Every(time.Second), 10))
		limiter := limiterAny.(*rate.Limiter)
		if !limiter.Allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func safeKeyPreview(key string) string {
	if len(key) < 8 {
		return "-"
	}
	return key[:8]
}

func clientIP(r *http.Request) string {
	// Caddy sits in front and sets X-Forwarded-For; fall back to RemoteAddr.
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	return r.RemoteAddr
}

func (s *Server) authenticate(r *http.Request) (string, bool, error) {
	key := r.Header.Get("X-API-Token")
	if key == "" {
		return "", false, nil
	}
	valid, err := s.st.ValidKey(key)
	if err != nil {
		return "", false, err
	}
	if !valid {
		log.Printf("DEBUG auth reject: len=%d key=%q", len(key), key)
	}
	return key, valid, nil
}

func (s *Server) handleGetContent(w http.ResponseWriter, r *http.Request) {
	key, valid, err := s.authenticate(r)
	if err != nil {
		log.Printf("auth error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !valid {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	blob, etag, err := s.st.GetBlob(key)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no data synced yet", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("get blob error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" && ifNoneMatch == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/octet-stream")
	// Set Content-Length explicitly (instead of letting net/http fall back to
	// chunked encoding) so a connection that drops mid-download fails loudly
	// on the client instead of silently decoding a truncated body.
	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) handlePutContent(w http.ResponseWriter, r *http.Request) {
	key, valid, err := s.authenticate(r)
	if err != nil {
		log.Printf("auth error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !valid {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBlobSize)
	blob, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if len(blob) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	ifMatch := r.Header.Get("If-Match")
	newEtag, err := s.st.PutBlob(key, blob, ifMatch)
	if errors.Is(err, store.ErrConflict) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if errors.Is(err, store.ErrShrinkGuard) {
		log.Printf("shrink guard rejected write: key=%s... new_size=%d", safeKeyPreview(key), len(blob))
		http.Error(w, "rejected: new data is suspiciously smaller than what's currently stored; "+
			"refusing to avoid data loss", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("put blob error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("ETag", newEtag)
	w.WriteHeader(http.StatusOK)
}

type syncEventPayload struct {
	Event      string `json:"event"`
	DeviceName string `json:"device_name"`
	Message    string `json:"message"`
}

func (s *Server) handlePostEvent(w http.ResponseWriter, r *http.Request) {
	key, valid, err := s.authenticate(r)
	if err != nil {
		log.Printf("auth error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !valid {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // events are tiny JSON
	var payload syncEventPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if err := s.st.LogEvent(key, payload.Event, payload.DeviceName, payload.Message); err != nil {
		log.Printf("log event error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
