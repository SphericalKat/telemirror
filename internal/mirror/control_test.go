package mirror_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
	"github.com/SphericalKat/telemirror/internal/telegram"
)

const startReply = "You should know the commands already. Happy mirroring."

const cancelHint = "Reply to the command message for the download that you want to cancel."

// startMirrorFrom sends an authorized mirror command from a named user and
// returns the request the engine accepted. The download stays queued.
func startMirrorFrom(t *testing.T, h *harness, chatID, userID int64, username, url string) (gid, dir string, cmdID int64) {
	t.Helper()

	cmdID = h.commandAs(t, chatID, userID, username, "/mirror "+url)
	adds := h.dl.added()
	if len(adds) == 0 {
		t.Fatal("no download added for the mirror command")
	}
	last := adds[len(adds)-1]
	if last.URL != url {
		t.Fatalf("added URL = %q, want %q", last.URL, url)
	}
	return fmt.Sprintf("gid%d", len(adds)), last.Dir, cmdID
}

// activateMirror drives one queued download to the active state.
func activateMirror(t *testing.T, h *harness, gid, dir string) {
	t.Helper()

	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:          engine.StatusActive,
		Dir:             dir,
		TotalLength:     1536,
		CompletedLength: 512,
		DownloadSpeed:   512,
		Files:           []engine.File{{Path: filepath.Join(dir, "file.bin"), Length: 1536, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventStart})
}

// commandTarget builds the original command message later replies target.
func commandTarget(chatID, messageID int64) telegram.Message {
	return telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: chatID, Type: "group"}}
}

// sendsReplyTo returns the messages sent as a reply to one command.
func sendsReplyTo(t *testing.T, h *harness, replyTo int64) []sentMessage {
	t.Helper()

	var found []sentMessage
	for _, sent := range h.tg.sends() {
		if sent.ReplyTo == replyTo {
			found = append(found, sent)
		}
	}
	return found
}

// sendTexts returns the messages sent to one chat.
func sendTexts(h *harness, chatID int64) []string {
	var texts []string
	for _, sent := range h.tg.sends() {
		if sent.ChatID == chatID {
			texts = append(texts, sent.Text)
		}
	}
	return texts
}

// wasDeleted reports whether one message was removed.
func wasDeleted(h *harness, chatID, messageID int64) bool {
	for _, deletion := range h.tg.deletions() {
		if deletion.ChatID == chatID && deletion.MessageID == messageID {
			return true
		}
	}
	return false
}

// dirRemoved reports whether the request's download directory is gone.
func dirRemoved(dir string) bool {
	_, err := os.Stat(dir)
	return os.IsNotExist(err)
}

func TestStartRepliesWithUpstreamHelp(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.TemporaryReplyDeleteDelay = 40 * time.Millisecond
	})

	cmdID := h.command(t, -100200, 42, "/start")

	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != startReply {
		t.Fatalf("replies = %+v, want one %q", replies, startReply)
	}

	// The reply is permanent: neither the reply nor the command is removed.
	time.Sleep(150 * time.Millisecond)
	if deletions := h.tg.deletions(); len(deletions) != 0 {
		t.Errorf("deletions = %+v, want none for a permanent reply", deletions)
	}
}

func TestStartUnauthorizedUserReceivesUpstreamResponse(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.TemporaryReplyDeleteDelay = 40 * time.Millisecond
	})

	cmdID := h.command(t, 555, 42, "/start")

	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != "You aren't authorized to use this bot here." {
		t.Fatalf("replies = %+v, want the unauthorized response", replies)
	}

	eventually(t, "unauthorized reply and command removed", func() bool {
		return wasDeleted(h, 555, replies[0].MessageID) && wasDeleted(h, 555, cmdID)
	})
}

func TestStartWithArgumentIsIgnored(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, -100200, 42, "/start now")

	if sends := h.tg.sends(); len(sends) != 0 {
		t.Errorf("replies sent = %d, want 0 for /start with an argument (last: %+v)", len(sends), sends[len(sends)-1])
	}
}

func TestMirrorStatusSendsOneOrderedStatusPerChat(t *testing.T) {
	h := newHarness(t, nil)
	_, _, _ = startAcceptedMirror(t, h, 42)
	_, _, _ = startMirrorFrom(t, h, -100200, 42, "kat", "http://files.example/media/two.bin")

	sends := h.tg.sends()
	if len(sends) != 2 {
		t.Fatalf("status messages sent = %d, want one per accepted request (%+v)", len(sends), sends)
	}
	existing := sends[1]

	cmdID := h.command(t, -100200, 42, "/mirrorStatus")

	if !wasDeleted(h, -100200, existing.MessageID) {
		t.Errorf("previous status message %d was not removed", existing.MessageID)
	}
	if !wasDeleted(h, -100200, cmdID) {
		t.Error("the /mirrorStatus command was not removed right away")
	}
	statuses := sendsReplyTo(t, h, cmdID)
	if len(statuses) != 1 {
		t.Fatalf("status messages replying to the command = %d, want 1 (%+v)", len(statuses), statuses)
	}
	text := statuses[0].Text
	active, queued := strings.Index(text, "file.bin"), strings.Index(text, "two.bin")
	if active < 0 || queued < 0 {
		t.Fatalf("status text = %q, want entries for both requests", text)
	}
	if active > queued {
		t.Errorf("status text = %q, want the first request listed first", text)
	}
}

func TestMirrorStatusWithNoDownloadsShowsEmptyStatus(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.StatusMessageTTL = 60 * time.Millisecond
	})

	cmdID := h.command(t, -100200, 42, "/mirrorStatus")

	statuses := sendsReplyTo(t, h, cmdID)
	if len(statuses) != 1 || statuses[0].Text != "No active or queued downloads" {
		t.Fatalf("status messages = %+v, want the empty status", statuses)
	}
	if !wasDeleted(h, -100200, cmdID) {
		t.Error("the /mirrorStatus command was not removed right away")
	}
	eventually(t, "status message removed after its lifetime", func() bool {
		return wasDeleted(h, -100200, statuses[0].MessageID)
	})
}

func TestMirrorStatusUnauthorizedUserReceivesUpstreamResponse(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, 555, 42, "/mirrorStatus")

	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != "You aren't authorized to use this bot here." {
		t.Fatalf("replies = %+v, want the unauthorized response", replies)
	}
}

func TestMirrorStatusMessageStopsUpdatingAfterLifetime(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.StatusMessageTTL = 60 * time.Millisecond
	})
	gid, dir, _ := startAcceptedMirror(t, h, 42)

	cmdID := h.command(t, -100200, 42, "/mirrorStatus")
	statuses := sendsReplyTo(t, h, cmdID)
	if len(statuses) != 1 {
		t.Fatalf("status messages = %+v, want one", statuses)
	}
	statusID := statuses[0].MessageID

	eventually(t, "status message removed after its lifetime", func() bool {
		return wasDeleted(h, -100200, statusID)
	})

	// Progress that changes after the lifetime must not edit the removed
	// message.
	info := engine.DownloadInfo{
		Status:          engine.StatusActive,
		Dir:             dir,
		TotalLength:     1536,
		CompletedLength: 1024,
		DownloadSpeed:   512,
		Files:           []engine.File{{Path: filepath.Join(dir, "file.bin"), Length: 1536, Selected: true}},
	}
	h.dl.setStatus(gid, info)
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventStart})
	time.Sleep(150 * time.Millisecond)

	for _, edit := range h.tg.edits() {
		if edit.MessageID == statusID {
			t.Errorf("edit %+v targeted the removed status message", edit)
		}
	}
}

func TestStatusEditsAreSuppressedWhenTextIsUnchanged(t *testing.T) {
	h := newHarness(t, nil)
	startAcceptedMirror(t, h, 42)

	eventually(t, "first active status edit", func() bool {
		for _, edit := range h.tg.edits() {
			if strings.Contains(edit.Text, "<b>Filename</b>") {
				return true
			}
		}
		return false
	})
	before := len(h.tg.edits())

	// Many refresh ticks pass without a change; none of them may edit.
	time.Sleep(150 * time.Millisecond)
	if after := len(h.tg.edits()); after != before {
		t.Errorf("edits grew from %d to %d while the status text did not change", before, after)
	}
}

func TestCancelMirrorByDownloadOwner(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	h.replyCommand(t, -100200, 42, "/cancelMirror", commandTarget(-100200, cmdID))

	eventually(t, "download cancelled", func() bool { return slices.Contains(h.dl.cancelled(), gid) })
	eventually(t, "stopped reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == "Download stopped." && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal", func() bool {
		return dirRemoved(dir)
	})
	if calls := h.tg.administratorCalls(); len(calls) != 0 {
		t.Errorf("administrator lookups = %v, want none for the download owner", calls)
	}
}

func TestCancelMirrorBySudoUser(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gid, dir)

	h.replyCommand(t, -100200, 9001, "/cancelMirror", commandTarget(-100200, cmdID))

	eventually(t, "download cancelled", func() bool { return slices.Contains(h.dl.cancelled(), gid) })
	eventually(t, "stopped reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == "Download stopped." && last.ReplyTo == cmdID
	})
	if calls := h.tg.administratorCalls(); len(calls) != 0 {
		t.Errorf("administrator lookups = %v, want none for a sudo user", calls)
	}
}

func TestCancelMirrorByChatAdministrator(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gid, dir)
	h.tg.setAdmins(-100200, 42, 9001)

	h.replyCommand(t, -100200, 42, "/cancelMirror", commandTarget(-100200, cmdID))

	eventually(t, "download cancelled", func() bool { return slices.Contains(h.dl.cancelled(), gid) })
	if calls := h.tg.administratorCalls(); len(calls) != 1 || calls[0] != -100200 {
		t.Errorf("administrator lookups = %v, want one for the origin chat", calls)
	}
}

func TestCancelMirrorByNonAdminMemberRejected(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gid, dir)
	h.tg.setAdmins(-100200, 9001)

	cancelCmdID := h.replyCommand(t, -100200, 42, "/cancelMirror", commandTarget(-100200, cmdID))

	replies := sendsReplyTo(t, h, cancelCmdID)
	want := "You do not have permission to do that."
	if len(replies) != 1 || replies[0].Text != want {
		t.Fatalf("replies = %+v, want %q", replies, want)
	}
	if cancels := h.dl.cancelled(); len(cancels) != 0 {
		t.Errorf("downloads cancelled = %v, want none", cancels)
	}
}

func TestCancelMirrorInAllAdministratorsChatSkipsLookup(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gid, dir)

	msgID := h.tg.newMessageID()
	up := update(-100200, msgID, 42, "/cancelMirror")
	up.Message.Chat.AllMembersAreAdministrators = true
	up.Message.ReplyToMessage = &telegram.Message{MessageID: cmdID, Chat: telegram.Chat{ID: -100200, Type: "group"}}
	h.svc.HandleUpdate(t.Context(), up)

	eventually(t, "download cancelled", func() bool { return slices.Contains(h.dl.cancelled(), gid) })
	if calls := h.tg.administratorCalls(); len(calls) != 0 {
		t.Errorf("administrator lookups = %v, want none when all members administrate", calls)
	}
}

func TestCancelMirrorByUnauthorizedUserRejected(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gid, dir)

	msgID := h.tg.newMessageID()
	up := update(555, msgID, 42, "/cancelMirror")
	up.Message.ReplyToMessage = &telegram.Message{MessageID: cmdID, Chat: telegram.Chat{ID: -100200, Type: "group"}}
	h.svc.HandleUpdate(t.Context(), up)

	last, ok := h.tg.lastSend()
	want := "You aren't authorized to use this bot here."
	if !ok || last.Text != want || last.ReplyTo != msgID {
		t.Fatalf("last reply = %+v, want %q replying to the command", last, want)
	}
	if cancels := h.dl.cancelled(); len(cancels) != 0 {
		t.Errorf("downloads cancelled = %v, want none", cancels)
	}
}

func TestCancelMirrorWithoutReplyAsksForReply(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, -100200, 42, "/cancelMirror")

	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != cancelHint {
		t.Fatalf("replies = %+v, want %q", replies, cancelHint)
	}
}

func TestCancelMirrorReplyToUnknownMessageAsksForActiveDownload(t *testing.T) {
	h := newHarness(t, nil)

	h.replyCommand(t, -100200, 42, "/cancelMirror", commandTarget(-100200, 9999))

	last, ok := h.tg.lastSend()
	want := cancelHint + " Also make sure that the download is even active."
	if !ok || last.Text != want {
		t.Fatalf("last reply = %+v, want %q", last, want)
	}
}

func TestCancelMirrorUploadInProgressRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.pub.setBlock()
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})
	eventually(t, "uploading status", func() bool {
		for _, edit := range h.tg.edits() {
			if strings.Contains(edit.Text, "<b>Uploading</b>") {
				return true
			}
		}
		return false
	})

	cancelCmdID := h.replyCommand(t, -100200, 42, "/cancelMirror", commandTarget(-100200, cmdID))

	want := "Upload in progress. Cannot cancel."
	last, ok := h.tg.lastSend()
	if !ok || last.Text != want || last.ReplyTo != cancelCmdID {
		t.Fatalf("last reply = %+v, want %q", last, want)
	}
	if cancels := h.dl.cancelled(); len(cancels) != 0 {
		t.Errorf("downloads cancelled = %v, want none for an uploading request", cancels)
	}

	// The upload itself continues undisturbed.
	h.pub.unblock()
	eventually(t, "completion reply after upload", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.Contains(last.Text, "<a href=")
	})
}

func TestCancelMirrorQueuedDownloadStopsWithoutEvent(t *testing.T) {
	h := newHarness(t, nil)
	gid, dir, cmdID := startMirrorFrom(t, h, -100200, 42, "kat", mirrorURL)

	h.replyCommand(t, -100200, 42, "/cancelMirror", commandTarget(-100200, cmdID))

	if cancels := h.dl.cancelled(); !slices.Contains(cancels, gid) {
		t.Fatalf("downloads cancelled = %v, want %s", cancels, gid)
	}
	eventually(t, "stopped reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == "Download stopped." && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal", func() bool {
		return dirRemoved(dir)
	})
}

func TestCancelAllBySudoUserCancelsEveryChat(t *testing.T) {
	h := newHarness(t, nil)
	gidA, dirA, _ := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gidA, dirA)
	gidB, dirB, _ := startMirrorFrom(t, h, 555, 9001, "sudoer", "http://files.example/media/two.bin")

	cmdID := h.commandAs(t, 555, 9001, "sudoer", "/cancelAll")

	eventually(t, "both downloads cancelled", func() bool {
		return slices.Contains(h.dl.cancelled(), gidA) && slices.Contains(h.dl.cancelled(), gidB)
	})
	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != "2 downloads cancelled." {
		t.Fatalf("replies = %+v, want the cancellation count", replies)
	}
	if texts := sendTexts(h, -100200); !slices.Contains(texts, "@alice, your downloads have been manually cancelled.") {
		t.Errorf("chat -100200 texts = %q, want the manual-cancellation notice", texts)
	}
	if texts := sendTexts(h, 555); !slices.Contains(texts, "@sudoer, your downloads have been manually cancelled.") {
		t.Errorf("chat 555 texts = %q, want the manual-cancellation notice", texts)
	}

	// The chat notices replace the per-download stopped replies.
	time.Sleep(150 * time.Millisecond)
	for _, sent := range h.tg.sends() {
		if sent.Text == "Download stopped." {
			t.Errorf("sent %q, want the manual-cancellation notice instead", sent.Text)
		}
	}
	eventually(t, "download directories removal", func() bool {
		return dirRemoved(dirA) && dirRemoved(dirB)
	})
}

func TestCancelAllByAllAdministratorsChatCancelsOnlyOriginChat(t *testing.T) {
	h := newHarness(t, nil)
	gidA, dirA, _ := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gidA, dirA)
	gidB, _, _ := startMirrorFrom(t, h, 555, 9001, "sudoer", "http://files.example/media/two.bin")

	msgID := h.tg.newMessageID()
	up := update(-100200, msgID, 42, "/cancelAll")
	up.Message.Chat.AllMembersAreAdministrators = true
	h.svc.HandleUpdate(t.Context(), up)

	eventually(t, "origin chat download cancelled", func() bool {
		return slices.Contains(h.dl.cancelled(), gidA)
	})
	replies := sendsReplyTo(t, h, msgID)
	if len(replies) != 1 || replies[0].Text != "1 downloads cancelled." {
		t.Fatalf("replies = %+v, want the cancellation count", replies)
	}
	if texts := sendTexts(h, -100200); !slices.Contains(texts, "@alice, your downloads have been manually cancelled.") {
		t.Errorf("chat -100200 texts = %q, want the manual-cancellation notice", texts)
	}
	if slices.Contains(h.dl.cancelled(), gidB) {
		t.Error("a chat administrator cancelled a request from another chat")
	}
	if texts := sendTexts(h, 555); slices.Contains(texts, "manually cancelled") {
		t.Errorf("chat 555 texts = %q, want no cancellation notice", texts)
	}
	if calls := h.tg.administratorCalls(); len(calls) != 0 {
		t.Errorf("administrator lookups = %v, want none when all members administrate", calls)
	}
}

func TestCancelAllByVerifiedAdministratorCancelsOnlyOriginChat(t *testing.T) {
	h := newHarness(t, nil)
	gidA, dirA, _ := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gidA, dirA)
	gidB, _, _ := startMirrorFrom(t, h, 555, 9001, "sudoer", "http://files.example/media/two.bin")
	h.tg.setAdmins(-100200, 42)

	cmdID := h.command(t, -100200, 42, "/cancelAll")

	eventually(t, "origin chat download cancelled", func() bool {
		return slices.Contains(h.dl.cancelled(), gidA)
	})
	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != "1 downloads cancelled." {
		t.Fatalf("replies = %+v, want the cancellation count", replies)
	}
	if slices.Contains(h.dl.cancelled(), gidB) {
		t.Error("a chat administrator cancelled a request from another chat")
	}
	if calls := h.tg.administratorCalls(); len(calls) != 1 || calls[0] != -100200 {
		t.Errorf("administrator lookups = %v, want one for the origin chat", calls)
	}
}

func TestCancelAllByNonAdminMemberRejected(t *testing.T) {
	h := newHarness(t, nil)
	gidA, dirA, _ := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gidA, dirA)
	h.tg.setAdmins(-100200, 9001)

	cmdID := h.command(t, -100200, 42, "/cancelAll")

	replies := sendsReplyTo(t, h, cmdID)
	want := "You do not have permission to do that."
	if len(replies) != 1 || replies[0].Text != want {
		t.Fatalf("replies = %+v, want %q", replies, want)
	}
	if cancels := h.dl.cancelled(); len(cancels) != 0 {
		t.Errorf("downloads cancelled = %v, want none", cancels)
	}
}

func TestCancelAllByUnauthorizedUserRejected(t *testing.T) {
	h := newHarness(t, nil)
	gidA, dirA, _ := startMirrorFrom(t, h, -100200, 77, "alice", mirrorURL)
	activateMirror(t, h, gidA, dirA)

	cmdID := h.command(t, 555, 42, "/cancelAll")

	replies := sendsReplyTo(t, h, cmdID)
	want := "You aren't authorized to use this bot here."
	if len(replies) != 1 || replies[0].Text != want {
		t.Fatalf("replies = %+v, want %q", replies, want)
	}
	if cancels := h.dl.cancelled(); len(cancels) != 0 {
		t.Errorf("downloads cancelled = %v, want none", cancels)
	}
}

func TestCancelAllWithoutDownloadsReportsNothingToCancel(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.TemporaryReplyDeleteDelay = 40 * time.Millisecond
	})

	cmdID := h.command(t, 555, 9001, "/cancelAll")

	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != "No downloads to cancel" {
		t.Fatalf("replies = %+v, want the empty-cancellation response", replies)
	}
	eventually(t, "reply and command removed", func() bool {
		return wasDeleted(h, 555, replies[0].MessageID) && wasDeleted(h, 555, cmdID)
	})
}

func TestCancelAllSkipsUploadingDownload(t *testing.T) {
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
	eventually(t, "uploading status", func() bool {
		for _, edit := range h.tg.edits() {
			if strings.Contains(edit.Text, "<b>Uploading</b>") {
				return true
			}
		}
		return false
	})

	allCmdID := h.command(t, 555, 9001, "/cancelAll")

	replies := sendsReplyTo(t, h, allCmdID)
	if len(replies) != 1 || replies[0].Text != "No downloads to cancel" {
		t.Fatalf("replies = %+v, want the empty-cancellation response", replies)
	}
	if cancels := h.dl.cancelled(); slices.Contains(cancels, gid) {
		t.Errorf("downloads cancelled = %v, an uploading request must not be cancelled", cancels)
	}

	// The upstream bot marks the request as notified even though it could
	// not cancel the upload, so the finished upload reports no result link.
	h.pub.unblock()
	eventually(t, "upload handling completes", func() bool {
		return dirRemoved(dir)
	})
	time.Sleep(150 * time.Millisecond)
	for _, sent := range h.tg.sends() {
		if strings.Contains(sent.Text, "<a href=") {
			t.Errorf("sent %q, a request marked for cancellation must not report its result", sent.Text)
		}
	}
}

func TestControlCommandsUseBotNameInGroups(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.CommandsUseBotName = true
		cfg.CommandBotName = "@telemirror_bot"
	})

	h.command(t, -100200, 42, "/start")
	if sends := h.tg.sends(); len(sends) != 0 {
		t.Fatalf("replies sent = %d, want 0 without the bot name (last: %+v)", len(sends), sends[len(sends)-1])
	}

	cmdID := h.command(t, -100200, 42, "/start@telemirror_bot")
	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != startReply {
		t.Fatalf("replies = %+v, want one %q", replies, startReply)
	}

	sendsBefore := len(h.tg.sends())
	h.command(t, 555, 9001, "/cancelAll")
	if sends := h.tg.sends(); len(sends) != sendsBefore {
		t.Errorf("replies sent = %d, want %d without the bot name", len(sends), sendsBefore)
	}

	cancelCmdID := h.command(t, 555, 9001, "/cancelAll@telemirror_bot")
	replies = sendsReplyTo(t, h, cancelCmdID)
	if len(replies) != 1 || replies[0].Text != "No downloads to cancel" {
		t.Fatalf("replies = %+v, want the empty-cancellation response", replies)
	}
}
