package mirror_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
	"github.com/SphericalKat/telemirror/internal/storage"
)

// fakeStore replaces the storage boundary. Tests script Load through
// loaded and can fail every operation.
type fakeStore struct {
	mu        sync.Mutex
	saved     []storage.StoredRequest
	deleted   []string
	saveErr   error
	deleteErr error
	loadErr   error
	loaded    []storage.StoredRequest
}

func (f *fakeStore) Save(req storage.StoredRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, req)
	return nil
}

func (f *fakeStore) Delete(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, dir)
	return nil
}

func (f *fakeStore) Load() ([]storage.StoredRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storage.StoredRequest(nil), f.loaded...), f.loadErr
}

func (f *fakeStore) deletedDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// lastSaved returns the newest saved request.
func (f *fakeStore) lastSaved() (storage.StoredRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 {
		return storage.StoredRequest{}, false
	}
	return f.saved[len(f.saved)-1], true
}

// savedStoredRequest builds one queued stored request owned by user 42 in
// the authorized chat.
func savedStoredRequest(url, dir string) storage.StoredRequest {
	return storage.StoredRequest{
		GID:             "2089a0fffffffffffffade",
		URL:             url,
		Dir:             dir,
		ChatID:          -100200,
		MessageID:       55,
		UserID:          42,
		Username:        "@kat",
		RepliedUsername: "",
		Started:         time.Now().Add(-time.Minute),
		Tar:             false,
		Uploading:       false,
	}
}

// newRecoveryHarness builds a service around a scripted store.
func newRecoveryHarness(t *testing.T, store mirror.Store) *harness {
	t.Helper()
	return newHarness(t, func(cfg *mirror.Config) {
		cfg.Store = store
	})
}

// lastReplyTo returns the newest message sent as a reply to messageID.
func lastReplyTo(h *harness, messageID int64) (sentMessage, bool) {
	sends := h.tg.sends()
	for i := len(sends) - 1; i >= 0; i-- {
		if sends[i].ReplyTo == messageID {
			return sends[i], true
		}
	}
	return sentMessage{}, false
}

func TestAcceptedRequestIsSavedForRecovery(t *testing.T) {
	store := &fakeStore{}
	h := newRecoveryHarness(t, store)
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	saved, ok := store.lastSaved()
	if !ok {
		t.Fatal("the accepted request was not saved")
	}
	if saved.GID != gid || saved.URL != mirrorURL || saved.Dir != dir ||
		saved.ChatID != -100200 || saved.UserID != 42 || saved.Username != "@kat" ||
		saved.Tar || saved.Uploading {
		t.Errorf("saved request = %+v, want the accepted request %s %s", saved, gid, mirrorURL)
	}
	if saved.MessageID != cmdID {
		t.Errorf("saved message ID = %d, want the original command %d for recovery replies", saved.MessageID, cmdID)
	}

	// A finished request is removed from storage.
	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})
	eventually(t, "stored request removed after completion", func() bool {
		dirs := store.deletedDirs()
		return len(dirs) == 1 && dirs[0] == dir
	})
}

func TestMirrorTarRequestSavesTarFlag(t *testing.T) {
	store := &fakeStore{}
	h := newRecoveryHarness(t, store)

	h.command(t, -100200, 42, "/mirrorTar "+mirrorURL)

	eventually(t, "tar request saved", func() bool {
		saved, ok := store.lastSaved()
		return ok && saved.Tar
	})
}

func TestUploadStartUpdatesSavedRequest(t *testing.T) {
	store := &fakeStore{}
	h := newRecoveryHarness(t, store)
	h.pub.setBlock()
	gid, dir, _ := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	eventually(t, "uploading request saved", func() bool {
		saved, ok := store.lastSaved()
		return ok && saved.Uploading
	})

	h.pub.unblock()
}

func TestRecoveredRequestResumesAndCompletes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recovered")
	saved := savedStoredRequest(mirrorURL, dir)
	store := &fakeStore{loaded: []storage.StoredRequest{saved}}
	h := newRecoveryHarness(t, store)

	// The stored request resumes through the downloader with its original
	// download directory.
	eventually(t, "recovered download re-added", func() bool {
		adds := h.dl.added()
		return len(adds) == 1 && adds[0].Kind == "url" && adds[0].URL == mirrorURL && adds[0].Dir == dir
	})

	// The origin chat receives a fresh status message that replies to the
	// original command message.
	eventually(t, "fresh recovered status message", func() bool {
		last, ok := lastReplyTo(h, saved.MessageID)
		return ok && last.ChatID == saved.ChatID && strings.Contains(last.Text, "- Queued")
	})

	// The recovered request is tracked again, so it completes like a fresh
	// request under its new GID.
	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus("gid1", engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: "gid1", Type: engine.EventComplete})

	eventually(t, "completion reply for the recovered request", func() bool {
		last, ok := lastReplyTo(h, saved.MessageID)
		return ok && last.Text == "<a href='https://drive.example/file-1'>file.bin</a> (100B)"
	})
	eventually(t, "recovered request directory removed", func() bool {
		return dirRemoved(dir)
	})
}

func TestRecoveredMagnetRequestReAddsAsMagnet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recovered")
	saved := savedStoredRequest(magnetURI, dir)
	store := &fakeStore{loaded: []storage.StoredRequest{saved}}
	h := newRecoveryHarness(t, store)

	eventually(t, "recovered magnet re-added", func() bool {
		adds := h.dl.added()
		return len(adds) == 1 && adds[0].Kind == "magnet" && adds[0].URL == magnetURI && adds[0].Dir == dir
	})
}

func TestRecoveredUploadingRequestMarkedFailed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	saved := savedStoredRequest(mirrorURL, dir)
	saved.Uploading = true
	store := &fakeStore{loaded: []storage.StoredRequest{saved}}
	h := newRecoveryHarness(t, store)

	want := "Upload failed. Interrupted by a restart."
	eventually(t, "interrupted upload marked failed", func() bool {
		last, ok := lastReplyTo(h, saved.MessageID)
		return ok && last.ChatID == saved.ChatID && last.Text == want
	})

	if adds := h.dl.added(); len(adds) != 0 {
		t.Errorf("downloads added = %d, want 0 for an interrupted upload", len(adds))
	}
	eventually(t, "interrupted upload directory removed", func() bool {
		return dirRemoved(dir)
	})
	eventually(t, "interrupted upload removed from storage", func() bool {
		dirs := store.deletedDirs()
		return len(dirs) == 1 && dirs[0] == dir
	})
}

func TestRecoveryWhenDownloaderRejectsRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recovered")
	saved := savedStoredRequest(mirrorURL, dir)
	store := &fakeStore{loaded: []storage.StoredRequest{saved}}
	h := newRecoveryHarness(t, store)
	h.dl.setAddErr(errors.New("engine refused"))

	want := "Failed to start the download. engine refused"
	eventually(t, "recovery failure reply", func() bool {
		last, ok := lastReplyTo(h, saved.MessageID)
		return ok && last.ChatID == saved.ChatID && last.Text == want
	})
	eventually(t, "failed recovery removed from storage", func() bool {
		dirs := store.deletedDirs()
		return len(dirs) == 1 && dirs[0] == dir
	})
}

func TestStorageErrorsDoNotStopMirroring(t *testing.T) {
	store := &fakeStore{
		saveErr:   errors.New("disk full"),
		deleteErr: errors.New("disk full"),
		loadErr:   errors.New("disk full"),
	}
	h := newRecoveryHarness(t, store)
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	eventually(t, "completion reply despite storage errors", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == "<a href='https://drive.example/file-1'>file.bin</a> (100B)" && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal despite storage errors", func() bool {
		return dirRemoved(dir)
	})
}

func TestRestartWithoutStoreRestoresNothing(t *testing.T) {
	// The first service runs without a store and accepts one request.
	first := newHarness(t, nil)
	startAcceptedMirror(t, first, 42)
	first.stop()

	// The second service runs without a store over the same download
	// directory and restores nothing.
	second := newHarness(t, func(cfg *mirror.Config) {
		cfg.DownloadDir = first.downloadDir
	})

	time.Sleep(100 * time.Millisecond)
	if adds := second.dl.added(); len(adds) != 0 {
		t.Errorf("downloads added = %d, want 0 without a store", len(adds))
	}
	if sends := second.tg.sends(); len(sends) != 0 {
		t.Errorf("messages sent = %d, want 0 without a store (last: %+v)", len(sends), sends[len(sends)-1])
	}
	second.stop()
}

func TestRestartWithSQLiteStoreRestoresActiveRequest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "telemirror.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}

	first := newHarness(t, func(cfg *mirror.Config) {
		cfg.Store = store
	})
	cmdID := first.command(t, -100200, 42, "/mirror "+mirrorURL)
	adds := first.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	dir := adds[0].Dir

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	if len(loaded) != 1 || loaded[0].Dir != dir || loaded[0].MessageID != cmdID {
		t.Fatalf("loaded requests = %+v, want the accepted request with dir %s", loaded, dir)
	}
	first.stop()

	// The restarted service shares the storage file and the download
	// directory, like one bot process after a restart.
	reopened, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open() after restart error = %v", err)
	}
	second := newHarness(t, func(cfg *mirror.Config) {
		cfg.Store = reopened
		cfg.DownloadDir = first.downloadDir
	})

	eventually(t, "recovered download re-added after restart", func() bool {
		secondAdds := second.dl.added()
		return len(secondAdds) == 1 && secondAdds[0].URL == mirrorURL && secondAdds[0].Dir == dir
	})
	eventually(t, "fresh status message after restart", func() bool {
		last, ok := lastReplyTo(second, cmdID)
		return ok && last.ChatID == -100200 && strings.Contains(last.Text, "- Queued")
	})

	// The restored request completes through the restarted service.
	file := second.writeFileOnDisk(t, dir, "file.bin", 100)
	second.dl.setStatus("gid1", engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	second.dl.emit(engine.Event{GID: "gid1", Type: engine.EventComplete})

	eventually(t, "completion reply after restart", func() bool {
		last, ok := lastReplyTo(second, cmdID)
		return ok && last.Text == "<a href='https://drive.example/file-1'>file.bin</a> (100B)"
	})
	eventually(t, "stored request removed after restart completion", func() bool {
		remaining, loadErr := reopened.Load()
		return loadErr == nil && len(remaining) == 0
	})
}
