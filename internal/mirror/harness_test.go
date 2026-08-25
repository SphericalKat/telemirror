package mirror_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/mirror"
	"github.com/SphericalKat/telemirror/internal/telegram"
)

// harness wires the mirror service to fakes and runs it for the test.
type harness struct {
	tg          *fakeTelegram
	dl          *fakeDownloader
	pub         *fakePublisher
	svc         *mirror.Service
	downloadDir string
}

func newHarness(t *testing.T, mutate func(*mirror.Config)) *harness {
	t.Helper()

	cfg := mirror.Config{
		SudoUsers:            []int64{9001},
		AuthorizedChats:      []int64{-100200},
		DownloadDir:          t.TempDir(),
		StatusUpdateInterval: 10 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	tg := &fakeTelegram{}
	dl := newFakeDownloader()
	pub := newFakePublisher(driveFileResult("file-1"))

	svc, err := mirror.New(cfg, tg, dl, pub)
	if err != nil {
		t.Fatalf("mirror.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("service Run did not return after cancellation")
		}
	})

	return &harness{tg: tg, dl: dl, pub: pub, svc: svc, downloadDir: cfg.DownloadDir}
}

func driveFileResult(id string) drive.Result {
	return drive.Result{
		DriveID:  id,
		Name:     "file.bin",
		IsFolder: false,
		Link:     "https://drive.example/" + id,
	}
}

// update builds a Telegram update around a message.
func update(chatID, messageID, userID int64, text string) telegram.Update {
	return updateAs(chatID, messageID, userID, "kat", text)
}

// updateAs builds a Telegram update around a message from a named user.
func updateAs(chatID, messageID, userID int64, username, text string) telegram.Update {
	return telegram.Update{
		Message: &telegram.Message{
			MessageID: messageID,
			From:      &telegram.User{ID: userID, Username: username, FirstName: "Kat"},
			Chat:      telegram.Chat{ID: chatID, Type: "group"},
			Text:      text,
		},
	}
}

// command sends a command message through the service and returns its
// message ID, which later replies must target.
func (h *harness) command(t *testing.T, chatID, userID int64, text string) int64 {
	t.Helper()
	return h.commandAs(t, chatID, userID, "kat", text)
}

// commandAs sends a command message from a named user.
func (h *harness) commandAs(t *testing.T, chatID, userID int64, username, text string) int64 {
	t.Helper()
	msgID := h.tg.newMessageID()
	h.svc.HandleUpdate(context.Background(), updateAs(chatID, msgID, userID, username, text))
	return msgID
}

// replyCommand sends a command message that replies to target and returns
// the command message ID.
func (h *harness) replyCommand(t *testing.T, chatID, userID int64, text string, target telegram.Message) int64 {
	t.Helper()
	msgID := h.tg.newMessageID()
	up := update(chatID, msgID, userID, text)
	up.Message.ReplyToMessage = &target
	h.svc.HandleUpdate(context.Background(), up)
	return msgID
}

// writeFileOnDisk creates the file a fake download claims to have produced.
func (h *harness) writeFileOnDisk(t *testing.T, dir, name string, size int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// eventually fails the test unless cond holds within the timeout.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
