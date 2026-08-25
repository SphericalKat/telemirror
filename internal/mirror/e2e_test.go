package mirror_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
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

func (s *recordingDriveService) ListChildren(_ context.Context, _ string, _ []string) ([]drive.Child, error) {
	return nil, nil
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

	tg := &fakeTelegram{}
	lister := newFakeLister()
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
