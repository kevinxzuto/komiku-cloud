package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"

	"komiku-cloud/internal/server"
	"komiku-cloud/internal/store"
)

func main() {
	dbPath := envOr("DB_PATH", "/data/komiku-cloud.db")

	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		label := ""
		if len(os.Args) > 2 {
			label = os.Args[2]
		}
		runKeygen(dbPath, label)
		return
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
