// Package storage stores mirror requests in a SQLite database so the bot
// can recover pending work after a restart.
//
// The database uses the pure-Go SQLite driver and is created when the
// configured path does not exist yet. Only queued and active mirror
// requests are stored; completed requests are removed.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// The pure-Go SQLite driver for database/sql.
	_ "modernc.org/sqlite"
)

// schemaVersion is the database schema version this build reads and
// writes. A database with a newer version cannot be migrated backwards.
const schemaVersion = 1

// StoredRequest holds the state of one mirror request that must survive a
// restart. The download directory identifies the stored request, because
// the download GID changes when torrent metadata moves the request to a
// followed child download or when a recovered request resumes under a new
// GID.
type StoredRequest struct {
	// GID is the download GID the request had when it was stored.
	GID string

	// URL is the mirror input: an HTTP(S) URL or a magnet link.
	URL string

	// Dir is the unique download directory of the request.
	Dir string

	// ChatID is the origin chat that started the request.
	ChatID int64

	// MessageID is the command message the recovery replies to.
	MessageID int64

	// UserID is the download owner.
	UserID int64

	// Username is the rendered owner name.
	Username string

	// RepliedUsername is the rendered user the command replied to.
	RepliedUsername string

	// Started is the acceptance time of the request.
	Started time.Time

	// Tar marks a /mirrorTar request.
	Tar bool

	// Uploading reports that the Drive upload was in progress when the
	// request was stored.
	Uploading bool
}

// Store persists mirror requests in a SQLite database.
type Store struct {
	db *sql.DB
}

// schemaV1 creates the table for schema version 1.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS mirror_requests (
	dir              TEXT PRIMARY KEY,
	gid              TEXT NOT NULL,
	url              TEXT NOT NULL,
	chat_id          INTEGER NOT NULL,
	message_id       INTEGER NOT NULL,
	user_id          INTEGER NOT NULL,
	username         TEXT NOT NULL,
	replied_username TEXT NOT NULL,
	started_ms       INTEGER NOT NULL,
	tar              INTEGER NOT NULL,
	uploading        INTEGER NOT NULL
)
`

// Open opens the SQLite database at path, creating the file and the schema
// when needed, and migrates an existing database. It fails when the file
// cannot be opened as a SQLite database or was written by a newer schema
// version.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("storage: open requires a database path")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	// One connection serializes access and avoids database-locked errors.
	db.SetMaxOpenConns(1)

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// migrate brings the database to schemaVersion. An empty or new database
// starts at version 0 and receives the current schema.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("storage: read schema version: %w", err)
	}
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("storage: database schema version %d is newer than the supported version %d", version, schemaVersion)
	}
	if _, err := s.db.Exec(schemaV1); err != nil {
		return fmt.Errorf("storage: create schema: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 1`); err != nil {
		return fmt.Errorf("storage: set schema version: %w", err)
	}
	return nil
}

// Save inserts or updates one stored mirror request.
func (s *Store) Save(req StoredRequest) error {
	_, err := s.db.Exec(`
		INSERT INTO mirror_requests
			(dir, gid, url, chat_id, message_id, user_id, username, replied_username, started_ms, tar, uploading)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dir) DO UPDATE SET
			gid = excluded.gid,
			url = excluded.url,
			chat_id = excluded.chat_id,
			message_id = excluded.message_id,
			user_id = excluded.user_id,
			username = excluded.username,
			replied_username = excluded.replied_username,
			started_ms = excluded.started_ms,
			tar = excluded.tar,
			uploading = excluded.uploading`,
		req.Dir, req.GID, req.URL, req.ChatID, req.MessageID, req.UserID,
		req.Username, req.RepliedUsername, req.Started.UnixMilli(), req.Tar, req.Uploading)
	if err != nil {
		return fmt.Errorf("storage: save request %s: %w", req.Dir, err)
	}
	return nil
}

// Delete removes the stored mirror request for one download directory.
// Deleting an unknown directory is not an error.
func (s *Store) Delete(dir string) error {
	if _, err := s.db.Exec(`DELETE FROM mirror_requests WHERE dir = ?`, dir); err != nil {
		return fmt.Errorf("storage: delete request %s: %w", dir, err)
	}
	return nil
}

// Load returns every stored mirror request, oldest first.
func (s *Store) Load() ([]StoredRequest, error) {
	rows, err := s.db.Query(`
		SELECT dir, gid, url, chat_id, message_id, user_id, username, replied_username, started_ms, tar, uploading
		FROM mirror_requests
		ORDER BY started_ms, dir`)
	if err != nil {
		return nil, fmt.Errorf("storage: load requests: %w", err)
	}
	defer rows.Close()

	var requests []StoredRequest
	for rows.Next() {
		var req StoredRequest
		var startedMS int64
		var tar, uploading bool
		if err := rows.Scan(&req.Dir, &req.GID, &req.URL, &req.ChatID, &req.MessageID,
			&req.UserID, &req.Username, &req.RepliedUsername, &startedMS, &tar, &uploading); err != nil {
			return nil, fmt.Errorf("storage: load requests: %w", err)
		}
		req.Started = time.UnixMilli(startedMS)
		req.Tar = tar
		req.Uploading = uploading
		requests = append(requests, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: load requests: %w", err)
	}
	return requests, nil
}

// Close closes the database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("storage: close: %w", err)
	}
	return nil
}
