package mirror_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
	"github.com/SphericalKat/telemirror/internal/storage"
)

// recordingDriveService is a fake Drive service for the real publisher.
type recordingDriveService struct {
	mu      sync.Mutex
	uploads []string
}

func (s *recordingDriveService) CreateFolder(_ context.Context, _, _ string) (string, error) {
	return "", fmt.Errorf("folders not expected in the HTTP flow")
}

func (s *recordingDriveService) UploadFile(_ context.Context, _, name, _, _ string, _ int64, _ func(int64)) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, name)
	return "e2e-file-1", nil
}

func (s *recordingDriveService) GrantPublicRead(_ context.Context, _ string) error {
	return nil
}

func (s *recordingDriveService) GrantReadAccess(_ context.Context, _, _ string) error {
	return nil
}

func (s *recordingDriveService) uploaded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.uploads...)
}

// TestServiceWithRealEngineAndPublisher runs one complete /mirror workflow
// with the embedded engine and the real Drive publisher. Only Telegram and
// the Drive API are faked.
func TestServiceWithRealEngineAndPublisher(t *testing.T) {
	payload := []byte("telemirror-end-to-end-payload-0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	downloadDir := t.TempDir()
	eng, err := engine.New(engine.Config{DownloadDir: downloadDir, MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("engine.New() error = %v", err)
	}

	driveSvc := &recordingDriveService{}
	pub, err := drive.NewPublisher(driveSvc, drive.Config{ParentFolderID: "parent-0"})
	if err != nil {
		t.Fatalf("drive.NewPublisher() error = %v", err)
	}
	lister := newFakeLister()

	tg := &fakeTelegram{}
	cfg := mirror.Config{
		SudoUsers:            []int64{42},
		AuthorizedChats:      []int64{-100200},
		DownloadDir:          downloadDir,
		StatusUpdateInterval: 10 * time.Millisecond,
	}
	svc, err := mirror.New(cfg, tg, eng, pub, lister)
	if err != nil {
		t.Fatalf("mirror.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = eng.Run(ctx) }()
	go func() { _ = svc.Run(ctx) }()

	// Wait for the engine to accept downloads.
	deadline := time.Now().Add(5 * time.Second)
	for {
		active, waiting, _ := eng.Stats()
		if active == 0 && waiting == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not become idle")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cmdID := tg.newMessageID()
	svc.HandleUpdate(ctx, update(-100200, cmdID, 42, "/mirror "+srv.URL+"/payload.bin"))

	var final sentMessage
	eventually(t, "completion reply", func() bool {
		last, ok := tg.lastSend()
		if !ok || last.ReplyTo != cmdID {
			return false
		}
		if strings.Contains(last.Text, "Failed") {
			t.Fatalf("mirror failed: %s", last.Text)
		}
		final = last
		return strings.HasPrefix(final.Text, "<a href=")
	})

	wantLink := drive.FileLink("e2e-file-1")
	want := fmt.Sprintf("<a href='%s'>payload.bin</a> (%dB)", wantLink, len(payload))
	if final.Text != want {
		t.Errorf("completion reply = %q, want %q", final.Text, want)
	}

	eventually(t, "download directory cleanup", func() bool {
		entries, err := os.ReadDir(downloadDir)
		return err == nil || len(entries) == 0
	})

	if uploaded := driveSvc.uploaded(); len(uploaded) != 1 || uploaded[0] != "payload.bin" {
		t.Errorf("uploaded files = %v, want [payload.bin]", uploaded)
	}
}

// TestRestartRecoversActiveRequestThroughEngine stops the bot while one
// download is active and proves that a restarted process recovers the
// stored request, resumes it through the embedded engine, and finishes it.
// Only Telegram and the Drive API are faked; the engine and the storage
// database are real.
func TestRestartRecoversActiveRequestThroughEngine(t *testing.T) {
	payload := []byte("telemirror-restart-recovery-payload")
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	// Unblock the handlers before the server close waits for them.
	defer release()

	baseDir := t.TempDir()
	dbPath := filepath.Join(baseDir, "telemirror.db")
	downloadDir := filepath.Join(baseDir, "downloads")
	if err := os.MkdirAll(downloadDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", downloadDir, err)
	}

	newProcess := func(tg *fakeTelegram) (*mirror.Service, *storage.Store, func(), func()) {
		t.Helper()
		store, err := storage.Open(dbPath)
		if err != nil {
			t.Fatalf("storage.Open() error = %v", err)
		}
		eng, err := engine.New(engine.Config{DownloadDir: downloadDir, MaxConcurrent: 1})
		if err != nil {
			t.Fatalf("engine.New() error = %v", err)
		}
		driveSvc := &recordingDriveService{}
		pub, err := drive.NewPublisher(driveSvc, drive.Config{ParentFolderID: "parent-0"})
		if err != nil {
			t.Fatalf("drive.NewPublisher() error = %v", err)
		}
		lister := newFakeLister()
		svc, err := mirror.New(mirror.Config{
			SudoUsers:            []int64{42},
			AuthorizedChats:      []int64{-100200},
			DownloadDir:          downloadDir,
			StatusUpdateInterval: 10 * time.Millisecond,
			Store:                store,
		}, tg, eng, pub, lister)
		if err != nil {
			t.Fatalf("mirror.New() error = %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		engineDone := make(chan error, 1)
		serviceDone := make(chan error, 1)
		go func() { engineDone <- eng.Run(ctx) }()
		go func() { serviceDone <- svc.Run(ctx) }()

		stop := func() {
			t.Helper()
			cancel()
			deadline := time.Now().Add(10 * time.Second)
			for _, done := range []<-chan error{engineDone, serviceDone} {
				select {
				case <-done:
				case <-time.After(time.Until(deadline)):
					t.Fatal("process did not stop after cancellation")
				}
			}
			if err := store.Close(); err != nil {
				t.Fatalf("store.Close() error = %v", err)
			}
		}
		waitStarted := func() {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			for {
				active, waiting, _ := eng.Stats()
				if active > 0 || waiting > 0 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("engine did not start the download")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		return svc, store, stop, waitStarted
	}

	// The first process accepts a mirror request and stops while the
	// download is active.
	firstTG := &fakeTelegram{}
	firstSvc, firstStore, stopFirst, waitStartedFirst := newProcess(firstTG)
	cmdID := firstTG.newMessageID()
	firstSvc.HandleUpdate(context.Background(), update(-100200, cmdID, 42, "/mirror "+srv.URL+"/payload.bin"))

	// The engine promotes the download as soon as it is added, so the
	// active state appears in the initial status message or in the first
	// edit, whichever comes first.
	statusShowsActive := func(tg *fakeTelegram) bool {
		for _, sent := range tg.sends() {
			if sent.ReplyTo == cmdID && strings.Contains(sent.Text, "<b>Filename</b>") {
				return true
			}
		}
		for _, edit := range tg.edits() {
			if strings.Contains(edit.Text, "<b>Filename</b>") {
				return true
			}
		}
		return false
	}
	eventually(t, "active download before the restart", func() bool {
		return statusShowsActive(firstTG)
	})
	waitStartedFirst()
	saved, err := firstStore.Load()
	if err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("stored requests = %d, want the active request before the restart", len(saved))
	}
	stopFirst()

	// The restarted process recovers the stored request, resumes it
	// through the embedded engine, and finishes it after the server
	// releases the response.
	secondTG := &fakeTelegram{}
	_, secondStore, stopSecond, _ := newProcess(secondTG)
	defer stopSecond()

	// The recovered request announces itself with a fresh status message
	// that replies to the original command, then shows the resumed
	// download.
	eventually(t, "fresh status message after the restart", func() bool {
		for _, sent := range secondTG.sends() {
			if sent.ReplyTo == cmdID {
				return true
			}
		}
		return false
	})
	eventually(t, "resumed download after the restart", func() bool {
		return statusShowsActive(secondTG)
	})

	release()
	want := fmt.Sprintf("<a href='%s'>payload.bin</a> (%dB)", drive.FileLink("e2e-file-1"), len(payload))
	eventually(t, "completion reply after the restart", func() bool {
		sends := secondTG.sends()
		for i := len(sends) - 1; i >= 0; i-- {
			if sends[i].ReplyTo == cmdID {
				return sends[i].Text == want
			}
		}
		return false
	})
	eventually(t, "stored request removed after the restart", func() bool {
		remaining, loadErr := secondStore.Load()
		return loadErr == nil && len(remaining) == 0
	})
}
