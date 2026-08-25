package mirror_test

import (
	"errors"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/mirror"
)

const diskReply = "Total space: 500GB\nUsed: 120GB\nAvailable: 380GB"

func TestDiskReportsSpaceInReplyToTheCommand(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.TemporaryReplyDeleteDelay = 40 * time.Millisecond
	})

	cmdID := h.command(t, -100200, 42, "/disk")

	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != diskReply {
		t.Fatalf("replies = %+v, want one %q", replies, diskReply)
	}
	if calls := h.disk.requested(); len(calls) != 1 || calls[0] != h.downloadDir {
		t.Errorf("reported paths = %v, want one call on the download directory %s", calls, h.downloadDir)
	}

	// The reply is temporary: both the reply and the command are removed.
	eventually(t, "disk reply and command removed", func() bool {
		return wasDeleted(h, -100200, replies[0].MessageID) && wasDeleted(h, -100200, cmdID)
	})
}

func TestDiskSudoUserCanUseItInAnyChat(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, 555, 9001, "/disk")

	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != diskReply {
		t.Fatalf("replies = %+v, want one %q", replies, diskReply)
	}
	if replies[0].ChatID != 555 {
		t.Errorf("reply chat = %d, want the command chat %d", replies[0].ChatID, 555)
	}
}

func TestDiskUnauthorizedUserReceivesUpstreamResponse(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, 555, 42, "/disk")

	want := "You aren't authorized to use this bot here."
	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != want {
		t.Fatalf("replies = %+v, want one %q", replies, want)
	}
	if calls := h.disk.requested(); len(calls) != 0 {
		t.Errorf("reported paths = %v, want none for an unauthorized user", calls)
	}
}

func TestDiskReportsTheConfiguredMountPoint(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.DiskRoot = "/srv/mirror"
	})

	h.command(t, -100200, 42, "/disk")

	if calls := h.disk.requested(); len(calls) != 1 || calls[0] != "/srv/mirror" {
		t.Errorf("reported paths = %v, want one call on the configured mount point /srv/mirror", calls)
	}
}

func TestDiskErrorProducesAClearReply(t *testing.T) {
	h := newHarness(t, nil)
	h.disk.setErr(errors.New("cannot read the mount point"))

	cmdID := h.command(t, -100200, 42, "/disk")

	want := "Failed to get disk space. cannot read the mount point"
	replies := sendsReplyTo(t, h, cmdID)
	if len(replies) != 1 || replies[0].Text != want {
		t.Fatalf("replies = %+v, want one %q", replies, want)
	}
}

func TestDiskWithArgumentIsIgnored(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, -100200, 42, "/disk now")

	if sends := h.tg.sends(); len(sends) != 0 {
		t.Errorf("replies sent = %d, want 0 for /disk with an argument (last: %+v)", len(sends), sends[len(sends)-1])
	}
	if calls := h.disk.requested(); len(calls) != 0 {
		t.Errorf("reported paths = %v, want none for /disk with an argument", calls)
	}
}
