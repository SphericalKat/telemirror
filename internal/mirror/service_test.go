package mirror_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
	"github.com/SphericalKat/telemirror/internal/telegram"
)

const mirrorURL = "http://files.example/media/file.bin"

// startAcceptedMirror sends an authorized /mirror command and drives the
// download until it is active. It returns the GID, the request directory, and
// the command message ID.
func startAcceptedMirror(t *testing.T, h *harness, userID int64) (gid, dir string, cmdID int64) {
	t.Helper()

	cmdID = h.command(t, -100200, userID, "/mirror "+mirrorURL)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	if adds[0].URL != mirrorURL {
		t.Errorf("added URL = %q, want %q", adds[0].URL, mirrorURL)
	}
	gid = "gid1"
	dir = adds[0].Dir
	if dir == "" || !strings.HasPrefix(dir, h.downloadDir+string(os.PathSeparator)) {
		t.Fatalf("download dir = %q, want a subdirectory of %s", dir, h.downloadDir)
	}

	// The accepted request shows a queued status that replies to the command.
	eventually(t, "queued status reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.ReplyTo == cmdID && strings.Contains(last.Text, "- Queued")
	})

	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:          engine.StatusActive,
		Dir:             dir,
		TotalLength:     1536,
		CompletedLength: 512,
		DownloadSpeed:   512,
		Files:           []engine.File{{Path: filepath.Join(dir, "file.bin"), Length: 1536, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventStart})

	eventually(t, "active status edit", func() bool {
		for _, e := range h.tg.edits() {
			if strings.Contains(e.Text, "<b>Filename</b>") && strings.Contains(e.Text, "file.bin") {
				return true
			}
		}
		return false
	})
	return gid, dir, cmdID
}

func TestMirrorDownloadsPublishesAndReplies(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "file.bin", 1536)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 1536,
		Files:       []engine.File{{Path: file, Length: 1536, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	eventually(t, "published result", func() bool { return len(h.pub.published()) == 1 })
	if got := h.pub.published()[0]; got != file {
		t.Errorf("published root = %q, want %q", got, file)
	}

	want := "<a href='https://drive.example/file-1'>file.bin</a> (1.5KB)"
	eventually(t, "completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want
	})
	last, _ := h.tg.lastSend()
	if last.ChatID != -100200 {
		t.Errorf("completion reply chat = %d, want the origin chat %d", last.ChatID, -100200)
	}
	if last.ReplyTo != cmdID {
		t.Errorf("completion reply target = %d, want the original command %d", last.ReplyTo, cmdID)
	}

	// The local download directory is removed after handling.
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorSudoUserCanMirrorInAnyChat(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, 555, 9001, "/mirror "+mirrorURL)

	eventually(t, "download for sudo user", func() bool { return len(h.dl.added()) == 1 })
}

func TestMirrorUnauthorizedUserReceivesUpstreamResponse(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, 555, 42, "/mirror "+mirrorURL)

	want := "You aren't authorized to use this bot here."
	eventually(t, "unauthorized reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID && last.ChatID == 555
	})
	if adds := h.dl.added(); len(adds) != 0 {
		t.Errorf("downloads added = %d, want 0 for an unauthorized user", len(adds))
	}
	if h.pub.published() != nil {
		t.Error("publish attempted for an unauthorized user")
	}
}

func TestMirrorBlockedDomainRejectedBeforeDownload(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.FilteredDomains = []string{"evil.example"}
	})
	cmdID := h.command(t, -100200, 42, "/mirror http://cdn.evil.example/file.bin")

	want := "Download failed. Blacklisted URL."
	eventually(t, "blocked domain reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if adds := h.dl.added(); len(adds) != 0 {
		t.Errorf("downloads added = %d, want 0 for a blocked domain", len(adds))
	}
}

func TestMirrorWithoutURLArgumentIsIgnored(t *testing.T) {
	h := newHarness(t, nil)

	for _, text := range []string{"/mirror", "/mirror ", "/mirrorstatus now"} {
		h.command(t, -100200, 42, text)
	}

	if sends := h.tg.sends(); len(sends) != 0 {
		t.Errorf("replies sent = %d, want 0 (last: %+v)", len(sends), sends[len(sends)-1])
	}
	if adds := h.dl.added(); len(adds) != 0 {
		t.Errorf("downloads added = %d, want 0 without a URL argument", len(adds))
	}
}

func TestMirrorCommandMatching(t *testing.T) {
	cases := []struct {
		name     string
		useName  bool
		chatType string
		text     string
		accepted bool
	}{
		{name: "lowercase", text: "/mirror " + mirrorURL, accepted: true},
		{name: "uppercase", text: "/MIRROR " + mirrorURL, accepted: true},
		{name: "mixed case", text: "/Mirror " + mirrorURL, accepted: true},
		{name: "any suffix allowed when name not required", text: "/mirror@somebot " + mirrorURL, accepted: true},
		{name: "wrong command", text: "/mirrorX " + mirrorURL, accepted: false},
		{name: "name required in group", useName: true, chatType: "group", text: "/mirror " + mirrorURL, accepted: false},
		{name: "configured name accepted in group", useName: true, chatType: "group", text: "/mirror@telemirror_bot " + mirrorURL, accepted: true},
		{name: "configured name matches without case", useName: true, chatType: "group", text: "/mirror@TeLeMirror_Bot " + mirrorURL, accepted: true},
		{name: "other name rejected in group", useName: true, chatType: "group", text: "/mirror@other_bot " + mirrorURL, accepted: false},
		{name: "name not required in private chat", useName: true, chatType: "private", text: "/mirror " + mirrorURL, accepted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatType := tc.chatType
			if chatType == "" {
				chatType = "group"
			}
			h := newHarness(t, func(cfg *mirror.Config) {
				cfg.CommandsUseBotName = tc.useName
				cfg.CommandBotName = "@telemirror_bot"
			})

			msgID := h.tg.newMessageID()
			up := update(-100200, msgID, 9001, tc.text)
			up.Message.Chat.Type = chatType
			h.svc.HandleUpdate(t.Context(), up)

			added := len(h.dl.added()) == 1
			if added != tc.accepted {
				t.Errorf("accepted = %v, want %v (sends: %+v)", added, tc.accepted, h.tg.sends())
			}
		})
	}
}

func TestMirrorUsesUniqueDirectoryPerRequest(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, -100200, 42, "/mirror http://a.example/one.bin")
	h.command(t, -100200, 42, "/mirror http://a.example/two.bin")

	adds := h.dl.added()
	if len(adds) != 2 {
		t.Fatalf("downloads added = %d, want 2", len(adds))
	}
	if adds[0].Dir == adds[1].Dir {
		t.Errorf("both requests used directory %q, want unique directories", adds[0].Dir)
	}
}

func TestMirrorDownloadFailureRepliesWithEngineError(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:       engine.StatusError,
		Dir:          dir,
		ErrorMessage: "connection refused",
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventError})

	want := "Failed to download. connection refused"
	eventually(t, "download failure reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("publish attempted after download failure: %v", published)
	}
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorUploadFailureRepliesWithDriveError(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {})
	h.pub.err = errors.New("disk quota exceeded")
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	want := "Failed to upload <code>file.bin</code> to Drive. disk quota exceeded"
	eventually(t, "upload failure reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorCompletionWithoutFilesFails(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	h.dl.setStatus(gid, engine.DownloadInfo{Status: engine.StatusComplete, Dir: dir})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	want := "Upload failed. Could not get files."
	eventually(t, "missing files reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("publish attempted without files: %v", published)
	}
}

func TestMirrorAddFailureRepliesAndCleansUp(t *testing.T) {
	h := newHarness(t, nil)
	h.dl.setAddErr(errors.New("engine refused"))
	cmdID := h.command(t, -100200, 42, "/mirror "+mirrorURL)

	want := "Failed to start the download. engine refused"
	eventually(t, "add failure reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("publish attempted after add failure: %v", published)
	}
}

func TestMirrorShowsUploadingStatus(t *testing.T) {
	h := newHarness(t, nil)
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

	eventually(t, "uploading status edit", func() bool {
		for _, e := range h.tg.edits() {
			if strings.Contains(e.Text, "<b>Uploading</b>") && strings.Contains(e.Text, "file.bin") {
				return true
			}
		}
		return false
	})

	h.pub.unblock()
	eventually(t, "completion reply after upload", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.HasPrefix(last.Text, "<a href=")
	})
}

func TestMirrorSharedDriveFolderNotice(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) { cfg.IsTeamDrive = true })
	h.pub.setResult(drive.Result{DriveID: "folder-1", Name: "pack", IsFolder: true, Link: "https://drive.example/folder-1"})
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	notice := "Folders in Shared Drives can only be shared with members of the drive."
	eventually(t, "shared drive notice", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.Contains(last.Text, notice) && last.ReplyTo == cmdID
	})
}

func TestMirrorCompletionCcRepliedUser(t *testing.T) {
	h := newHarness(t, nil)

	msgID := h.tg.newMessageID()
	up := update(-100200, msgID, 42, "/mirror "+mirrorURL)
	up.Message.ReplyToMessage = &telegram.Message{
		MessageID: 55,
		From:      &telegram.User{ID: 77, Username: "alice", FirstName: "Alice"},
	}
	h.svc.HandleUpdate(t.Context(), up)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	gid, dir := "gid1", adds[0].Dir

	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusActive,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventStart})
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	eventually(t, "completion reply with cc", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.Contains(last.Text, "\ncc: @alice")
	})
}
