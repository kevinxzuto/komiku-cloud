package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"komiku-cloud/internal/server"
	"komiku-cloud/internal/store"
)

func main() {
	dbPath := envOr("DB_PATH", "/data/komiku-cloud.db")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "keygen":
			label := ""
			if len(os.Args) > 2 {
				label = os.Args[2]
			}
			runKeygen(dbPath, label)
			return
		case "history":
			if len(os.Args) < 3 {
				log.Fatal("usage: komiku-cloud history <api-key>")
			}
			runHistory(dbPath, os.Args[2])
			return
		case "restore":
			if len(os.Args) < 4 {
				log.Fatal("usage: komiku-cloud restore <api-key> <history-id>")
			}
			id, err := strconv.ParseInt(os.Args[3], 10, 64)
			if err != nil {
				log.Fatalf("invalid history id: %v", err)
			}
			runRestore(dbPath, os.Args[2], id)
			return
		}
	}

	runServe(dbPath)
}

func runServe(dbPath string) {
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	addr := envOr("LISTEN_ADDR", ":8080")
	srv := server.New(st)

	log.Printf("komiku-cloud listening on %s (db: %s)", addr, dbPath)
	if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func runKeygen(dbPath, label string) {
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	key, err := generateKey()
	if err != nil {
		log.Fatalf("failed to generate key: %v", err)
	}

	if err := st.CreateAPIKey(key, label); err != nil {
		log.Fatalf("failed to save key: %v", err)
	}

	fmt.Println(key)
}

func runHistory(dbPath, apiKey string) {
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	entries, err := st.ListHistory(apiKey)
	if err != nil {
		log.Fatalf("failed to list history: %v", err)
	}
	if len(entries) == 0 {
		fmt.Println("no history entries for this key")
		return
	}
	for _, e := range entries {
		t := time.Unix(e.SavedAt, 0).Format(time.RFC3339)
		fmt.Printf("id=%d  saved_at=%s  size=%d bytes\n", e.ID, t, e.Size)
	}
}

func runRestore(dbPath, apiKey string, historyID int64) {
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	newEtag, err := st.RestoreHistory(apiKey, historyID)
	if err != nil {
		log.Fatalf("failed to restore: %v", err)
	}
	fmt.Printf("restored history id=%d, new etag=%s\n", historyID, newEtag)
}

func generateKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
