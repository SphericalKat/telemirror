package storage_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/storage"
)

// storedRequest builds one stored request with a unique directory.
func storedRequest(dir string, started time.Time) storage.StoredRequest {
	return storage.StoredRequest{
		GID:             "gid1",
		URL:             "http://files.example/media/file.bin",
		Dir:             dir,
		ChatID:          -100200,
		MessageID:       55,
		UserID:          42,
		Username:        "@kat",
		RepliedUsername: "@alice",
		Started:         started,
		Tar:             true,
		Uploading:       false,
	}
}

// assertEqual compares two stored requests field by field. The start time
// keeps only millisecond precision in the database.
func assertEqual(t *testing.T, got, want storage.StoredRequest) {
	t.Helper()
	want.Started = want.Started.Truncate(time.Millisecond)
	if got.URL != want.URL || got.GID != want.GID || got.Dir != want.Dir ||
		got.ChatID != want.ChatID || got.MessageID != want.MessageID ||
		got.UserID != want.UserID || got.Username != want.Username ||
		got.RepliedUsername != want.RepliedUsername || got.Tar != want.Tar ||
		got.Uploading != want.Uploading || got.Started.UnixMilli() != want.Started.UnixMilli() {
		t.Errorf("loaded request = %+v, want %+v", got, want)
	}
}

func TestOpenCreatesDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemirror.db")
	store, err := storage.Open(path)
	if err != nil {
		t.Fatalf("storage.Open(%q) error = %v", path, err)
	}
	defer store.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file was not created: %v", err)
	}
}

func TestOpenWithoutPathFails(t *testing.T) {
	if _, err := storage.Open("   "); err == nil {
		t.Error("storage.Open with no path succeeded, want an error")
	}
}

func TestSaveAndLoadRequests(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "telemirror.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	first := storedRequest("/downloads/one", time.Now().Add(-2*time.Second))
	second := storedRequest("/downloads/two", time.Now())
	second.GID = "gid2"
	second.URL = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	second.Tar = false
	second.Uploading = true
	for _, req := range []storage.StoredRequest{second, first} {
		if err := store.Save(req); err != nil {
			t.Fatalf("Save(%q) error = %v", req.Dir, err)
		}
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded requests = %d, want 2", len(loaded))
	}
	assertEqual(t, loaded[0], first)
	assertEqual(t, loaded[1], second)
}

func TestSaveUpdatesExistingRequest(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "telemirror.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	original := storedRequest("/downloads/one", time.Now())
	if err := store.Save(original); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	updated := original
	updated.GID = "gid9"
	updated.Uploading = true
	if err := store.Save(updated); err != nil {
		t.Fatalf("Save() update error = %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded requests = %d, want the single updated request", len(loaded))
	}
	assertEqual(t, loaded[0], updated)
}

func TestDeleteRemovesRequest(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "telemirror.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	req := storedRequest("/downloads/one", time.Now())
	if err := store.Save(req); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Delete("/downloads/one"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("loaded requests = %d, want 0 after the delete", len(loaded))
	}

	// Deleting an unknown request is not an error.
	if err := store.Delete("/downloads/unknown"); err != nil {
		t.Errorf("Delete() of an unknown request = %v, want no error", err)
	}
}

func TestOpenFailsOnFileThatIsNoDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemirror.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database at all"), 0o600); err != nil {
		t.Fatalf("write corrupt database file: %v", err)
	}
	if _, err := storage.Open(path); err == nil {
		t.Error("storage.Open() on a non-database file succeeded, want an error")
	}
}

func TestOpenFailsWhenPathIsADirectory(t *testing.T) {
	path := t.TempDir()
	if _, err := storage.Open(path); err == nil {
		t.Error("storage.Open() on a directory succeeded, want an error")
	}
}

func TestOpenFailsOnNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemirror.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database directly: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set future schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if _, err := storage.Open(path); err == nil {
		t.Error("storage.Open() on a newer schema succeeded, want an error")
	}
}

func TestCloseAllowsReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemirror.db")
	first, err := storage.Open(path)
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	req := storedRequest("/downloads/one", time.Now())
	if err := first.Save(req); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second, err := storage.Open(path)
	if err != nil {
		t.Fatalf("storage.Open() after Close() error = %v", err)
	}
	defer second.Close()
	loaded, err := second.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded requests = %d, want the saved request to survive the reopen", len(loaded))
	}
	assertEqual(t, loaded[0], req)
}
