package mirror_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/mirror"
)

// int64p makes a size pointer for a drive.Child.
func int64p(n int64) *int64 { return &n }

func TestListRepliesWithMatchingChildren(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.setResults([]drive.Child{
		{ID: "file-1", Name: "movie.mkv", Size: int64p(1536), Link: drive.FileLink("file-1")},
		{ID: "folder-1", Name: "pack", IsFolder: true, Link: drive.FolderLink("folder-1")},
		{ID: "file-2", Name: "doc", Link: drive.FileLink("file-2")},
	})

	cmdID := h.command(t, -100200, 42, "/list movie")

	want := strings.Join([]string{
		"<a href = '" + drive.FileLink("file-1") + "'>movie.mkv</a> (1.5KB)",
		"<a href = '" + drive.FolderLink("folder-1") + "'>pack</a> (folder)",
		"<a href = '" + drive.FileLink("file-2") + "'>doc</a>",
		"",
	}, "\n")
	eventually(t, "list reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID && last.ChatID == -100200
	})

	if got := h.lister.searched(); len(got) != 1 || got[0] != "movie" {
		t.Errorf("searched names = %v, want [movie]", got)
	}
}

func TestListKeepsSearchSpacesForTheDriveSearch(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, -100200, 42, "/list movie night")

	eventually(t, "drive search", func() bool {
		return len(h.lister.searched()) == 1
	})
	if got := h.lister.searched()[0]; got != "movie night" {
		t.Errorf("searched name = %q, want %q with the spaces preserved", got, "movie night")
	}
}

func TestListNoResultsRepliesUpstreamMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.setResults(nil)

	cmdID := h.command(t, -100200, 42, "/list absent")

	want := "There are no files matching your parameters"
	eventually(t, "no-results reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
}

func TestListDriveErrorRepliesWithFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.setErr(errors.New("drive unavailable"))

	cmdID := h.command(t, -100200, 42, "/list movie")

	want := "Failed to fetch the list of files"
	eventually(t, "failure reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
}

func TestListRequiresSearchTerm(t *testing.T) {
	h := newHarness(t, nil)

	for _, text := range []string{"/list", "/list "} {
		h.command(t, -100200, 42, text)
	}

	if sends := h.tg.sends(); len(sends) != 0 {
		t.Errorf("replies sent = %d, want 0 (last: %+v)", len(sends), sends[len(sends)-1])
	}
	if got := h.lister.searched(); len(got) != 0 {
		t.Errorf("drive searches = %v, want none", got)
	}
}

func TestListUnauthorizedUserReceivesUpstreamResponse(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, 555, 42, "/list movie")

	want := "You aren't authorized to use this bot here."
	eventually(t, "unauthorized reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID && last.ChatID == 555
	})
	if got := h.lister.searched(); len(got) != 0 {
		t.Errorf("drive searches = %v, want none for an unauthorized user", got)
	}
}

func TestGetFolderRepliesWithConfiguredFolderLink(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, -100200, 42, "/getFolder")

	want := "<a href = 'https://drive.example/folder'>Drive mirror folder</a>"
	eventually(t, "folder link reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID && last.ChatID == -100200
	})
}

func TestGetFolderIgnoresArguments(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, -100200, 42, "/getFolder now")

	if sends := h.tg.sends(); len(sends) != 0 {
		t.Errorf("replies sent = %d, want 0 (last: %+v)", len(sends), sends[len(sends)-1])
	}
}

func TestGetFolderUnauthorizedUserReceivesUpstreamResponse(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, 555, 42, "/getFolder")

	want := "You aren't authorized to use this bot here."
	eventually(t, "unauthorized reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID && last.ChatID == 555
	})
}

func TestListRemainsAvailableWithoutBotName(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.CommandsUseBotName = true
		cfg.CommandBotName = "@telemirror_bot"
	})

	// Group chats accept /list without the configured bot name, with any
	// other bot name, and private chats accept it plainly, so every bot in
	// a group can answer file searches.
	h.command(t, -100200, 42, "/list movie")
	h.command(t, -100200, 42, "/list@other_bot movie night")

	msgID := h.tg.newMessageID()
	up := update(-100200, msgID, 42, "/list movie")
	up.Message.Chat.Type = "private"
	h.svc.HandleUpdate(t.Context(), up)

	eventually(t, "list searches", func() bool {
		return len(h.lister.searched()) == 3
	})
	want := []string{"movie", "movie night", "movie"}
	if got := h.lister.searched(); !equalStrings(got, want) {
		t.Errorf("searched names = %v, want %v", got, want)
	}
}

func TestGetFolderFollowsBotNameRuleInGroups(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.CommandsUseBotName = true
		cfg.CommandBotName = "@telemirror_bot"
	})

	h.command(t, -100200, 42, "/getFolder")
	if sends := h.tg.sends(); len(sends) != 0 {
		t.Fatalf("replies sent = %d, want 0 without the bot name", len(sends))
	}

	h.command(t, -100200, 42, "/getFolder@telemirror_bot")
	eventually(t, "folder link reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.Contains(last.Text, "Drive mirror folder")
	})
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
