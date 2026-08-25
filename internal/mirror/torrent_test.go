package mirror_test

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
)

const (
	magnetURI  = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=pack&tr=http%3A%2F%2Ftracker.example%2Fannounce"
	torrentURL = "http://tracker.example/pack.torrent"
)

// startTorrentMetadata sends a mirror command for a torrent input and drives
// the metadata download until it completes with its followed child download.
// It returns the metadata GID, the child GID, the request directory, and the
// command message ID.
func startTorrentMetadata(t *testing.T, h *harness, userID int64, text string) (metaGID, childGID, dir string, cmdID int64) {
	t.Helper()

	cmdID = h.command(t, -100200, userID, text)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	dir = adds[0].Dir
	if dir == "" || !strings.HasPrefix(dir, h.downloadDir+string(os.PathSeparator)) {
		t.Fatalf("download dir = %q, want a subdirectory of %s", dir, h.downloadDir)
	}

	metaGID, childGID = "gid1", "gid2"

	// The accepted request shows a queued status that replies to the command.
	eventually(t, "queued status reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.ReplyTo == cmdID && strings.Contains(last.Text, "- Queued")
	})

	// The metadata download uses the same active status block as HTTP(S).
	h.dl.setStatus(metaGID, engine.DownloadInfo{
		Status:          engine.StatusActive,
		Dir:             dir,
		TotalLength:     4096,
		CompletedLength: 1024,
		DownloadSpeed:   512,
		Files:           []engine.File{{Path: filepath.Join(dir, "pack.torrent")}},
	})
	h.dl.emit(engine.Event{GID: metaGID, Type: engine.EventStart})
	eventually(t, "active metadata status edit", func() bool {
		for _, e := range h.tg.edits() {
			if strings.Contains(e.Text, "<b>Filename</b>") && strings.Contains(e.Text, "Metadata") {
				return true
			}
		}
		return false
	})

	// The metadata completes and hands over to the child download that holds
	// the real torrent files.
	h.dl.setStatus(metaGID, engine.DownloadInfo{
		Status:     engine.StatusComplete,
		Dir:        dir,
		FollowedBy: []string{childGID},
		Files:      []engine.File{{Path: filepath.Join(dir, "pack.torrent")}},
	})
	h.dl.emit(engine.Event{GID: metaGID, Type: engine.EventComplete, FollowedBy: []string{childGID}})
	return metaGID, childGID, dir, cmdID
}

// activateTorrentFiles points the tracked record at the finished child
// download and shows the real torrent content in the status message.
func activateTorrentFiles(t *testing.T, h *harness, childGID, dir, name string, total int64, paths ...string) {
	t.Helper()

	files := make([]engine.File, 0, len(paths))
	for _, p := range paths {
		files = append(files, engine.File{Path: p, Length: total / int64(len(paths)), Selected: true})
	}
	h.dl.setStatus(childGID, engine.DownloadInfo{
		Status:          engine.StatusActive,
		Dir:             dir,
		TotalLength:     total,
		CompletedLength: total / 2,
		DownloadSpeed:   512,
		Files:           files,
	})
	h.dl.emit(engine.Event{GID: childGID, Type: engine.EventStart})

	eventually(t, "active torrent status edit", func() bool {
		for _, e := range h.tg.edits() {
			if strings.Contains(e.Text, "<b>Filename</b>") && strings.Contains(e.Text, name) {
				return true
			}
		}
		return false
	})
}

// completeTorrent finishes the child download with the given on-disk files.
func completeTorrent(t *testing.T, h *harness, childGID, dir string, total int64, paths ...string) {
	t.Helper()

	files := make([]engine.File, 0, len(paths))
	for _, p := range paths {
		files = append(files, engine.File{Path: p, Length: total / int64(len(paths)), Selected: true})
	}
	h.dl.setStatus(childGID, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: total,
		Files:       files,
	})
	h.dl.emit(engine.Event{GID: childGID, Type: engine.EventComplete})
}

func TestMirrorMagnetLinkQueuesTorrentDownload(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, -100200, 42, "/mirror "+magnetURI)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	if adds[0].Kind != "magnet" {
		t.Errorf("add kind = %q, want a magnet download", adds[0].Kind)
	}
	if adds[0].URL != magnetURI {
		t.Errorf("added magnet = %q, want %q", adds[0].URL, magnetURI)
	}
	if !strings.HasPrefix(adds[0].Dir, h.downloadDir+string(os.PathSeparator)) {
		t.Errorf("download dir = %q, want a subdirectory of %s", adds[0].Dir, h.downloadDir)
	}
}

func TestMirrorRemoteTorrentURLQueuesMetadataDownload(t *testing.T) {
	h := newHarness(t, nil)

	h.command(t, -100200, 42, "/mirror "+torrentURL)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	if adds[0].Kind != "url" {
		t.Errorf("add kind = %q, want a URL download for a remote torrent file", adds[0].Kind)
	}
	if adds[0].URL != torrentURL {
		t.Errorf("added URL = %q, want %q", adds[0].URL, torrentURL)
	}
}

func TestMirrorRemoteTorrentTransitionsMetadataAndPublishesFiles(t *testing.T) {
	h := newHarness(t, nil)
	_, childGID, dir, cmdID := startTorrentMetadata(t, h, 42, "/mirror "+torrentURL)

	packDir := filepath.Join(dir, "pack")
	fileA := h.writeFileOnDisk(t, packDir, "file1.bin", 1024)
	fileB := h.writeFileOnDisk(t, filepath.Join(packDir, "sub"), "file2.bin", 512)

	activateTorrentFiles(t, h, childGID, dir, "pack", 1536, fileA, fileB)
	completeTorrent(t, h, childGID, dir, 1536, fileA, fileB)

	eventually(t, "published torrent directory", func() bool { return len(h.pub.published()) == 1 })
	if got := h.pub.published()[0]; got != packDir {
		t.Errorf("published root = %q, want the torrent directory %q", got, packDir)
	}

	want := "<a href='https://drive.example/file-1'>pack</a> (1.5KB)"
	eventually(t, "torrent completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorMagnetTransitionsMetadataAndPublishesFiles(t *testing.T) {
	h := newHarness(t, nil)
	_, childGID, dir, cmdID := startTorrentMetadata(t, h, 42, "/mirror "+magnetURI)

	// A single-file torrent places its file directly in the request dir.
	file := h.writeFileOnDisk(t, dir, "magnet.bin", 2048)

	activateTorrentFiles(t, h, childGID, dir, "magnet.bin", 2048, file)
	completeTorrent(t, h, childGID, dir, 2048, file)

	eventually(t, "published magnet content", func() bool { return len(h.pub.published()) == 1 })
	if got := h.pub.published()[0]; got != file {
		t.Errorf("published root = %q, want %q", got, file)
	}

	want := "<a href='https://drive.example/file-1'>magnet.bin</a> (2KB)"
	eventually(t, "magnet completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
}

func TestMirrorTorrentChildAlreadyCompleteWhenMetadataIsProcessed(t *testing.T) {
	h := newHarness(t, nil)

	cmdID := h.command(t, -100200, 42, "/mirror "+magnetURI)
	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	dir := adds[0].Dir

	file := h.writeFileOnDisk(t, filepath.Join(dir, "pack"), "magnet.bin", 512)

	// The child finishes before the record moves to its GID, so the
	// completion event for the child is dropped. The transition must still
	// publish the result.
	h.dl.setStatus("gid1", engine.DownloadInfo{
		Status:     engine.StatusComplete,
		Dir:        dir,
		FollowedBy: []string{"gid2"},
		Files:      []engine.File{{Path: filepath.Join(dir, "pack.torrent")}},
	})
	h.dl.setStatus("gid2", engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 512,
		Files:       []engine.File{{Path: file, Length: 512, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: "gid1", Type: engine.EventComplete, FollowedBy: []string{"gid2"}})
	h.dl.emit(engine.Event{GID: "gid2", Type: engine.EventComplete})

	eventually(t, "published after missed child event", func() bool { return len(h.pub.published()) == 1 })
	eventually(t, "completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.HasPrefix(last.Text, "<a href=") && last.ReplyTo == cmdID
	})
}

func TestMirrorTorrentFailureUsesSharedReplyBehavior(t *testing.T) {
	h := newHarness(t, nil)
	_, childGID, dir, cmdID := startTorrentMetadata(t, h, 42, "/mirror "+magnetURI)

	h.dl.setStatus(childGID, engine.DownloadInfo{
		Status:       engine.StatusError,
		Dir:          dir,
		ErrorMessage: "tracker refused the announce",
	})
	h.dl.emit(engine.Event{GID: childGID, Type: engine.EventError})

	want := "Failed to download. tracker refused the announce"
	eventually(t, "torrent failure reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("publish attempted after torrent failure: %v", published)
	}
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorTorrentMetadataWithoutFollowIsNotPublished(t *testing.T) {
	h := newHarness(t, nil)
	cmdID := h.command(t, -100200, 42, "/mirror "+torrentURL)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	gid, dir := "gid1", adds[0].Dir

	// The metadata file completes without a followed child download.
	metadata := h.writeFileOnDisk(t, dir, "pack.torrent", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: metadata, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	want := "Upload failed. Could not get files."
	eventually(t, "metadata-only completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("torrent metadata published: %v", published)
	}
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorFilenameFilterPreventsPublication(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.FilteredFilenames = []string{"secret"}
	})
	gid, dir, cmdID := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "secret.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	want := "Upload failed. Blacklisted file name."
	eventually(t, "blocked filename reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("publish attempted for a blocked filename: %v", published)
	}
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorFilenameFilterIsCaseSensitive(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.FilteredFilenames = []string{"SECRET"}
	})
	gid, dir, _ := startAcceptedMirror(t, h, 42)

	file := h.writeFileOnDisk(t, dir, "secret.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	eventually(t, "completion reply for a case-different name", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.HasPrefix(last.Text, "<a href=")
	})
	if published := h.pub.published(); len(published) != 1 {
		t.Errorf("published = %v, want the download despite the case difference", published)
	}
}

func TestMirrorFilenameFilterStopsActiveDownload(t *testing.T) {
	h := newHarness(t, func(cfg *mirror.Config) {
		cfg.FilteredFilenames = []string{"yify"}
	})
	cmdID := h.command(t, -100200, 42, "/mirror "+mirrorURL)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	gid, dir := "gid1", adds[0].Dir

	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:          engine.StatusActive,
		Dir:             dir,
		TotalLength:     2048,
		CompletedLength: 512,
		DownloadSpeed:   512,
		Files:           []engine.File{{Path: filepath.Join(dir, "yify-pack.mkv"), Length: 2048, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventStart})

	want := "Download stopped. Blacklisted file name."
	eventually(t, "stopped download reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	if cancelled := h.dl.cancelledGIDs(); len(cancelled) != 1 || cancelled[0] != gid {
		t.Errorf("cancelled GIDs = %v, want [%s]", cancelled, gid)
	}
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("publish attempted for a blocked download: %v", published)
	}
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorTarArchivesDirectoryResult(t *testing.T) {
	h := newHarness(t, nil)
	cmdID := h.command(t, -100200, 42, "/mirrorTar "+mirrorURL)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	gid, dir := "gid1", adds[0].Dir

	packDir := filepath.Join(dir, "pack")
	fileA := h.writeFileOnDisk(t, packDir, "file1.bin", 700)
	fileB := h.writeFileOnDisk(t, filepath.Join(packDir, "sub"), "file2.bin", 300)

	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 1000,
		Files:       []engine.File{{Path: fileA, Length: 700, Selected: true}, {Path: fileB, Length: 300, Selected: true}},
	})

	// Hold the publisher so the archive can be inspected before cleanup.
	h.pub.setBlock()
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	eventually(t, "published archive", func() bool { return len(h.pub.published()) == 1 })
	archivePath := filepath.Join(dir, "pack.tar")
	if got := h.pub.published()[0]; got != archivePath {
		t.Errorf("published root = %q, want the archive %q", got, archivePath)
	}
	verifyTarContents(t, archivePath, map[string]int64{
		"pack/file1.bin":     700,
		"pack/sub/file2.bin": 300,
	})
	h.pub.unblock()

	wantPrefix := "<a href='https://drive.example/file-1'>pack.tar</a> ("
	eventually(t, "archived completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.HasPrefix(last.Text, wantPrefix) && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorTarSingleFileIsNotArchived(t *testing.T) {
	h := newHarness(t, nil)
	cmdID := h.command(t, -100200, 42, "/mirrorTar "+mirrorURL)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	gid, dir := "gid1", adds[0].Dir

	file := h.writeFileOnDisk(t, dir, "file.bin", 100)
	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	eventually(t, "published single file", func() bool { return len(h.pub.published()) == 1 })
	if got := h.pub.published()[0]; got != file {
		t.Errorf("published root = %q, want the file itself %q", got, file)
	}

	want := "<a href='https://drive.example/file-1'>file.bin</a> (100B)"
	eventually(t, "single file completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && last.Text == want && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

func TestMirrorTarArchiveFailureRepliesAndCleansUp(t *testing.T) {
	h := newHarness(t, nil)
	cmdID := h.command(t, -100200, 42, "/mirrorTar "+mirrorURL)

	adds := h.dl.added()
	if len(adds) != 1 {
		t.Fatalf("downloads added = %d, want 1", len(adds))
	}
	gid, dir := "gid1", adds[0].Dir

	file := h.writeFileOnDisk(t, filepath.Join(dir, "pack"), "file1.bin", 100)
	// A directory at the archive path makes archive creation fail.
	if err := os.Mkdir(filepath.Join(dir, "pack.tar"), 0o700); err != nil {
		t.Fatalf("mkdir blocking archive path: %v", err)
	}

	h.dl.setStatus(gid, engine.DownloadInfo{
		Status:      engine.StatusComplete,
		Dir:         dir,
		TotalLength: 100,
		Files:       []engine.File{{Path: file, Length: 100, Selected: true}},
	})
	h.dl.emit(engine.Event{GID: gid, Type: engine.EventComplete})

	wantPrefix := "Failed to upload <code>pack.tar</code> to Drive. "
	eventually(t, "archive failure reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.HasPrefix(last.Text, wantPrefix) && last.ReplyTo == cmdID
	})
	if published := h.pub.published(); len(published) != 0 {
		t.Errorf("publish attempted after archive failure: %v", published)
	}
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

// verifyTarContents reads a created archive and checks that it holds exactly
// the given files, each with the given size, inside the top-level "pack"
// directory.
func verifyTarContents(t *testing.T, path string, wantFiles map[string]int64) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive %s: %v", path, err)
	}
	defer f.Close()

	gotFiles := map[string]int64{}
	sawTopDir := false
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		if header.Typeflag == tar.TypeDir {
			if header.Name == "pack/" {
				sawTopDir = true
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			t.Errorf("archive entry %q has type %q, want a regular file", header.Name, string(header.Typeflag))
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s content: %v", header.Name, err)
		}
		gotFiles[header.Name] = int64(len(data))
	}

	if !sawTopDir {
		t.Error("archive does not contain the top-level pack/ directory entry")
	}
	if len(gotFiles) != len(wantFiles) {
		t.Errorf("archive files = %v, want exactly %v", gotFiles, wantFiles)
	}
	for name, size := range wantFiles {
		if got, ok := gotFiles[name]; !ok || got != size {
			t.Errorf("archive file %q = %d bytes (present: %v), want %d", name, got, ok, size)
		}
	}
}

func TestMirrorTarMagnetArchivesTorrentDirectory(t *testing.T) {
	h := newHarness(t, nil)
	_, childGID, dir, cmdID := startTorrentMetadata(t, h, 42, "/mirrorTar "+magnetURI)

	packDir := filepath.Join(dir, "pack")
	fileA := h.writeFileOnDisk(t, packDir, "file1.bin", 512)
	fileB := h.writeFileOnDisk(t, packDir, "file2.bin", 512)

	activateTorrentFiles(t, h, childGID, dir, "pack", 1024, fileA, fileB)

	// Hold the publisher so the archive can be inspected before cleanup.
	h.pub.setBlock()
	completeTorrent(t, h, childGID, dir, 1024, fileA, fileB)

	eventually(t, "published torrent archive", func() bool { return len(h.pub.published()) == 1 })
	archivePath := filepath.Join(dir, "pack.tar")
	if got := h.pub.published()[0]; got != archivePath {
		t.Errorf("published root = %q, want the archive %q", got, archivePath)
	}
	verifyTarContents(t, archivePath, map[string]int64{
		"pack/file1.bin": 512,
		"pack/file2.bin": 512,
	})
	h.pub.unblock()

	wantPrefix := "<a href='https://drive.example/file-1'>pack.tar</a> ("
	eventually(t, "torrent archive completion reply", func() bool {
		last, ok := h.tg.lastSend()
		return ok && strings.HasPrefix(last.Text, wantPrefix) && last.ReplyTo == cmdID
	})
	eventually(t, "download directory removal", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}
