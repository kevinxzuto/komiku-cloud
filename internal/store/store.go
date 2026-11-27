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
var ErrShrinkGuard = errors.New("rejected: new data is suspiciously smaller than what's stored")

// shrinkGuardMinCurrentSize: only guard once there's a non-trivial amount of
// data stored, so small/legitimate backups aren't blocked.
const shrinkGuardMinCurrentSize = 50 * 1024

// shrinkGuardRatio: reject a write that shrinks the blob to less than this
// fraction of its current size. This is a blunt but effective defense
// against a client silently overwriting good remote data with an empty or
// near-empty local state after a failed download/decode (seen in practice:
// Komikku's sync client falls back to "just push local" on any decode
// error instead of refusing to sync).
const shrinkGuardRatio = 0.2

// historyKeepPerKey: how many prior blob versions to retain per API key so
// an accidental overwrite (shrink-guard false negative, or a deliberate
// force-push) can still be rolled back.
const historyKeepPerKey = 20

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
		CREATE TABLE IF NOT EXISTS sync_blobs_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			api_key TEXT NOT NULL,
			blob BLOB NOT NULL,
			etag TEXT NOT NULL,
			saved_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_history_key_time ON sync_blobs_history (api_key, saved_at);
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
	var currentBlob []byte
	err = tx.QueryRow(`SELECT etag, blob FROM sync_blobs WHERE api_key = ?`, key).Scan(&currentEtag, &currentBlob)
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

	if hasCurrent && len(currentBlob) >= shrinkGuardMinCurrentSize &&
		float64(len(blob)) < float64(len(currentBlob))*shrinkGuardRatio {
		return "", ErrShrinkGuard
	}

	newEtag := sha256Hex(blob)
	now := time.Now().Unix()

	if hasCurrent {
		// Snapshot the outgoing version to history before it's replaced, so
		// a bad write (or a future shrink-guard false negative) is always
		// recoverable.
		if _, err = tx.Exec(
			`INSERT INTO sync_blobs_history (api_key, blob, etag, saved_at) VALUES (?, ?, ?, ?)`,
			key, currentBlob, currentEtag, now,
		); err != nil {
			return "", err
		}
		if _, err = tx.Exec(
			`DELETE FROM sync_blobs_history WHERE api_key = ? AND id NOT IN (
				SELECT id FROM sync_blobs_history WHERE api_key = ? ORDER BY saved_at DESC LIMIT ?
			)`,
			key, key, historyKeepPerKey,
		); err != nil {
			return "", err
		}

		_, err = tx.Exec(
			`UPDATE sync_blobs SET blob = ?, etag = ?, updated_at = ? WHERE api_key = ?`,
			blob, newEtag, now, key,
		)
	} else {
		_, err = tx.Exec(
			`INSERT INTO sync_blobs (api_key, blob, etag, updated_at) VALUES (?, ?, ?, ?)`,
			key, blob, newEtag, now,
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

// HistoryEntry describes one previously-stored blob version.
type HistoryEntry struct {
	ID      int64
	SavedAt int64
	Size    int
}

// ListHistory returns prior blob versions for a key, most recent first.
func (s *Store) ListHistory(key string) ([]HistoryEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, saved_at, length(blob) FROM sync_blobs_history WHERE api_key = ? ORDER BY saved_at DESC`,
		key,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		if err := rows.Scan(&h.ID, &h.SavedAt, &h.Size); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RestoreHistory overwrites the current blob for a key with a prior history
// entry's contents (by history id), returning the new etag. The restored
// version is itself snapshotted first, so this is non-destructive.
func (s *Store) RestoreHistory(key string, historyID int64) (string, error) {
	var blob []byte
	err := s.db.QueryRow(
		`SELECT blob FROM sync_blobs_history WHERE id = ? AND api_key = ?`, historyID, key,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	// Bypass the shrink guard deliberately: restoring is an explicit,
	// operator-initiated action.
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var currentEtag string
	var currentBlob []byte
	err = tx.QueryRow(`SELECT etag, blob FROM sync_blobs WHERE api_key = ?`, key).Scan(&currentEtag, &currentBlob)
	hasCurrent := !errors.Is(err, sql.ErrNoRows)
	if err != nil && hasCurrent {
		return "", err
	}

	newEtag := sha256Hex(blob)
	now := time.Now().Unix()

	if hasCurrent {
		if _, err = tx.Exec(
			`INSERT INTO sync_blobs_history (api_key, blob, etag, saved_at) VALUES (?, ?, ?, ?)`,
			key, currentBlob, currentEtag, now,
		); err != nil {
			return "", err
		}
		_, err = tx.Exec(
			`UPDATE sync_blobs SET blob = ?, etag = ?, updated_at = ? WHERE api_key = ?`,
			blob, newEtag, now, key,
		)
	} else {
		_, err = tx.Exec(
			`INSERT INTO sync_blobs (api_key, blob, etag, updated_at) VALUES (?, ?, ?, ?)`,
			key, blob, newEtag, now,
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
