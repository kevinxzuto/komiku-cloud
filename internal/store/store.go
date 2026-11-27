package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")
var ErrUnauthorized = errors.New("unauthorized")
var ErrConflict = errors.New("etag conflict")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite + WAL: keep writes serialized, avoids "database is locked"

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS api_keys (
			key TEXT PRIMARY KEY,
			label TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sync_blobs (
			api_key TEXT PRIMARY KEY REFERENCES api_keys(key),
			blob BLOB NOT NULL,
			etag TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sync_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			api_key TEXT NOT NULL,
			event TEXT NOT NULL,
			device_name TEXT,
			message TEXT,
			created_at INTEGER NOT NULL
		);
	`); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// CreateAPIKey inserts a new API key with the given label and returns it.
func (s *Store) CreateAPIKey(key, label string) error {
	_, err := s.db.Exec(
		`INSERT INTO api_keys (key, label, created_at) VALUES (?, ?, ?)`,
		key, label, time.Now().Unix(),
	)
	return err
}

// ValidKey reports whether an API key exists.
func (s *Store) ValidKey(key string) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM api_keys WHERE key = ?`, key).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetBlob returns the stored blob and its etag for an api key.
// Returns ErrNotFound if nothing has been synced yet.
func (s *Store) GetBlob(key string) ([]byte, string, error) {
	var blob []byte
	var etag string
	err := s.db.QueryRow(`SELECT blob, etag FROM sync_blobs WHERE api_key = ?`, key).Scan(&blob, &etag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return blob, etag, nil
}

// PutBlob stores a new blob for the api key, enforcing the ifMatch precondition
// (mirrors HTTP If-Match semantics). ifMatch == "" means "no precondition, just write".
// Returns the new etag, or ErrConflict if ifMatch didn't match the current etag.
func (s *Store) PutBlob(key string, blob []byte, ifMatch string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var currentEtag string
	err = tx.QueryRow(`SELECT etag FROM sync_blobs WHERE api_key = ?`, key).Scan(&currentEtag)
	hasCurrent := true
	if errors.Is(err, sql.ErrNoRows) {
		hasCurrent = false
	} else if err != nil {
		return "", err
	}

	if ifMatch != "" {
		if !hasCurrent || currentEtag != ifMatch {
			return "", ErrConflict
		}
	}

	newEtag := sha256Hex(blob)

	if hasCurrent {
		_, err = tx.Exec(
			`UPDATE sync_blobs SET blob = ?, etag = ?, updated_at = ? WHERE api_key = ?`,
			blob, newEtag, time.Now().Unix(), key,
		)
	} else {
		_, err = tx.Exec(
			`INSERT INTO sync_blobs (api_key, blob, etag, updated_at) VALUES (?, ?, ?, ?)`,
			key, blob, newEtag, time.Now().Unix(),
		)
	}
	if err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return newEtag, nil
}

// LogEvent records a sync lifecycle event reported by a client.
func (s *Store) LogEvent(key, event, deviceName, message string) error {
	_, err := s.db.Exec(
		`INSERT INTO sync_events (api_key, event, device_name, message, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, event, deviceName, message, time.Now().Unix(),
	)
	return err
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
