package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		DownloadDir:   t.TempDir(),
		MaxConcurrent: 2,
	}
}

func TestHTTPDownloadLifecycle(t *testing.T) {
	payload := []byte("telemirror-http-payload-0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()

	e := startEngine(t, testConfig(t))
	events, stop := e.Events()
	defer stop()

	gid, err := e.AddURL(srv.URL+"/file.bin", nil)
	if err != nil {
		t.Fatalf("AddURL() error = %v", err)
	}
	if gid == "" {
		t.Fatal("AddURL() returned empty GID")
	}

	waitForEvent(t, events, gid, EventStart, 15*time.Second)
	waitForEvent(t, events, gid, EventComplete, 15*time.Second)

	info, err := e.Status(gid)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if info.Status != StatusComplete {
		t.Fatalf("Status = %q, want %q", info.Status, StatusComplete)
	}
	if info.TotalLength != int64(len(payload)) {
		t.Errorf("TotalLength = %d, want %d", info.TotalLength, len(payload))
	}
	if info.CompletedLength != int64(len(payload)) {
		t.Errorf("CompletedLength = %d, want %d", info.CompletedLength, len(payload))
	}
	if info.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty", info.ErrorMessage)
	}

	size, err := e.Size(gid)
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("Size() = %d, want %d", size, len(payload))
	}

	files, err := e.Files(gid)
	if err != nil {
		t.Fatalf("Files() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(files))
	}
	if !files[0].Selected || files[0].Length != int64(len(payload)) {
		t.Errorf("file[0] = %+v", files[0])
	}
	if got := readFile(t, files[0].Path); !bytes.Equal(got, payload) {
		t.Errorf("downloaded content mismatch: %q", got)
	}
	if filepath.Dir(files[0].Path) != info.Dir {
		t.Errorf("file dir = %s, want %s", filepath.Dir(files[0].Path), info.Dir)
	}
}

func TestHTTPSDownload(t *testing.T) {
	payload := []byte("telemirror-https-payload")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "secure.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.CACertPath = writeCAFile(t, srv)

	e := startEngine(t, cfg)
	events, stop := e.Events()
	defer stop()

	gid, err := e.AddURL(srv.URL+"/secure.bin", nil)
	if err != nil {
		t.Fatalf("AddURL() error = %v", err)
	}
	waitForEvent(t, events, gid, EventComplete, 15*time.Second)

	info, err := e.Status(gid)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if info.Status != StatusComplete {
		t.Fatalf("Status = %q, ErrorMessage = %q", info.Status, info.ErrorMessage)
	}
}

func TestPerDownloadDirectory(t *testing.T) {
	payload := []byte("telemirror-per-dir-payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "original.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()

	base := t.TempDir()
	e := startEngine(t, Config{DownloadDir: base, MaxConcurrent: 2})
	events, stop := e.Events()
	defer stop()

	subdir := filepath.Join(base, "request-uuid")
	gid, err := e.AddURL(srv.URL+"/original.bin", &AddOptions{Dir: subdir})
	if err != nil {
		t.Fatalf("AddURL() error = %v", err)
	}
	waitForEvent(t, events, gid, EventComplete, 15*time.Second)

	if got := readFile(t, filepath.Join(subdir, "original.bin")); !bytes.Equal(got, payload) {
		t.Errorf("per-download dir content mismatch: %q", got)
	}
}

func TestMagnetDownload(t *testing.T) {
	payload := []byte("telemirror-magnet-payload-abcdefghij")
	fixture := startBTFixture(t, "magnet.bin", payload, 16*1024)

	e := startEngine(t, testConfig(t))
	events, stop := e.Events()
	defer stop()

	gid, err := e.AddMagnet(fixture.MagnetURI(), nil)
	if err != nil {
		t.Fatalf("AddMagnet() error = %v", err)
	}

	ev := waitForEvent(t, events, gid, EventComplete, 30*time.Second)
	if len(ev.FollowedBy) != 1 {
		t.Fatalf("complete event FollowedBy = %v, want one child GID", ev.FollowedBy)
	}
	child := ev.FollowedBy[0]

	info, err := e.Status(gid)
	if err != nil {
		t.Fatalf("Status(metadata) error = %v", err)
	}
	if len(info.FollowedBy) != 1 || info.FollowedBy[0] != child {
		t.Fatalf("Status FollowedBy = %v, want [%s]", info.FollowedBy, child)
	}

	waitForEvent(t, events, child, EventComplete, 30*time.Second)

	childInfo, err := e.Status(child)
	if err != nil {
		t.Fatalf("Status(child) error = %v", err)
	}
	if childInfo.Status != StatusComplete {
		t.Fatalf("child status = %q, error = %q", childInfo.Status, childInfo.ErrorMessage)
	}
	if childInfo.Following != gid {
		t.Errorf("child Following = %q, want %q", childInfo.Following, gid)
	}
	if childInfo.TotalLength != int64(len(payload)) {
		t.Errorf("child TotalLength = %d, want %d", childInfo.TotalLength, len(payload))
	}

	files, err := e.Files(child)
	if err != nil {
		t.Fatalf("Files(child) error = %v", err)
	}
	if len(files) != 1 || filepath.Base(files[0].Path) != "magnet.bin" {
		t.Fatalf("child files = %+v", files)
	}
	if got := readFile(t, files[0].Path); !bytes.Equal(got, payload) {
		t.Errorf("magnet payload mismatch: %q", got)
	}
}

func TestTorrentFileBytes(t *testing.T) {
	payload := []byte("telemirror-torrent-payload-0123456789")
	fixture := startBTFixture(t, "bytes.bin", payload, 16*1024)

	e := startEngine(t, testConfig(t))
	events, stop := e.Events()
	defer stop()

	gid, err := e.AddTorrent(fixture.TorrentData, nil)
	if err != nil {
		t.Fatalf("AddTorrent() error = %v", err)
	}
	waitForEvent(t, events, gid, EventComplete, 30*time.Second)

	info, err := e.Status(gid)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if info.Status != StatusComplete {
		t.Fatalf("status = %q, error = %q", info.Status, info.ErrorMessage)
	}
	if info.InfoHash != fixtureHex(t, fixture.InfoHash[:]) {
		t.Errorf("InfoHash = %q, want fixture info hash", info.InfoHash)
	}

	files, err := e.Files(gid)
	if err != nil {
		t.Fatalf("Files() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(files))
	}
	if got := readFile(t, files[0].Path); !bytes.Equal(got, payload) {
		t.Errorf("torrent payload mismatch: %q", got)
	}
}

func fixtureHex(t *testing.T, b []byte) string {
	t.Helper()
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, digits[v>>4], digits[v&0xf])
	}
	return string(out)
}

func TestRemoteTorrentURL(t *testing.T) {
	payload := []byte("telemirror-remote-torrent-payload-ABCDE")
	fixture := startBTFixture(t, "remote.bin", payload, 16*1024)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		http.ServeContent(w, r, "payload.torrent", time.Now(), bytes.NewReader(fixture.TorrentData))
	}))
	defer srv.Close()

	e := startEngine(t, testConfig(t))
	events, stop := e.Events()
	defer stop()

	gid, err := e.AddURL(srv.URL+"/payload.torrent", nil)
	if err != nil {
		t.Fatalf("AddURL() error = %v", err)
	}

	ev := waitForEvent(t, events, gid, EventComplete, 30*time.Second)
	if len(ev.FollowedBy) != 1 {
		t.Fatalf("complete event FollowedBy = %v, want one child GID", ev.FollowedBy)
	}
	child := ev.FollowedBy[0]

	waitForEvent(t, events, child, EventComplete, 30*time.Second)

	info, err := e.Status(child)
	if err != nil {
		t.Fatalf("Status(child) error = %v", err)
	}
	if info.Status != StatusComplete {
		t.Fatalf("child status = %q, error = %q", info.Status, info.ErrorMessage)
	}
	if got := readFile(t, filepath.Join(info.Dir, "remote.bin")); !bytes.Equal(got, payload) {
		t.Errorf("remote torrent payload mismatch: %q", got)
	}
}

func TestCancelQueued(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseNow()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block.bin" {
			select {
			case <-release:
				http.ServeContent(w, r, "block.bin", time.Now(), bytes.NewReader([]byte("blocked")))
			case <-r.Context().Done():
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.MaxConcurrent = 1
	e := startEngine(t, cfg)
	events, stop := e.Events()
	defer stop()

	first, err := e.AddURL(srv.URL+"/block.bin", nil)
	if err != nil {
		t.Fatalf("AddURL(first) error = %v", err)
	}
	waitForEvent(t, events, first, EventStart, 15*time.Second)

	second, err := e.AddURL(srv.URL+"/after.bin", nil)
	if err != nil {
		t.Fatalf("AddURL(second) error = %v", err)
	}
	info, err := e.Status(second)
	if err != nil {
		t.Fatalf("Status(second) error = %v", err)
	}
	if info.Status != StatusWaiting {
		t.Fatalf("second status = %q, want %q", info.Status, StatusWaiting)
	}

	if err := e.Cancel(second); err != nil {
		t.Fatalf("Cancel(queued) error = %v", err)
	}
	if _, err := e.Status(second); !errors.Is(err, ErrNotFound) {
		t.Errorf("Status(cancelled) error = %v, want ErrNotFound", err)
	}
	if err := e.Cancel(second); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel(unknown) error = %v, want ErrNotFound", err)
	}

	releaseNow()
	waitForEvent(t, events, first, EventComplete, 15*time.Second)
}

func TestCancelActive(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			http.ServeContent(w, r, "block.bin", time.Now(), bytes.NewReader([]byte("blocked")))
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	e := startEngine(t, testConfig(t))
	events, stop := e.Events()
	defer stop()

	gid, err := e.AddURL(srv.URL+"/block.bin", nil)
	if err != nil {
		t.Fatalf("AddURL() error = %v", err)
	}
	waitForEvent(t, events, gid, EventStart, 15*time.Second)

	if err := e.Cancel(gid); err != nil {
		t.Fatalf("Cancel(active) error = %v", err)
	}
	waitForEvent(t, events, gid, EventStop, 15*time.Second)

	info, err := e.Status(gid)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if info.Status != StatusRemoved {
		t.Errorf("Status = %q, want %q", info.Status, StatusRemoved)
	}
}

func TestErrorReporting(t *testing.T) {
	e := startEngine(t, testConfig(t))
	events, stop := e.Events()
	defer stop()

	gid, err := e.AddURL("http://127.0.0.1:1/unreachable.bin", nil)
	if err != nil {
		t.Fatalf("AddURL() error = %v", err)
	}
	waitForEvent(t, events, gid, EventError, 30*time.Second)

	info, err := e.Status(gid)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if info.Status != StatusError {
		t.Errorf("Status = %q, want %q", info.Status, StatusError)
	}
	if info.ErrorMessage == "" {
		t.Error("ErrorMessage empty, want details")
	}
}

func TestContextStop(t *testing.T) {
	cfg := testConfig(t)
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- e.Run(ctx)
	}()

	deadline := time.After(5 * time.Second)
	for {
		active, waiting, _ := e.Stats()
		if active == 0 && waiting == 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run returned early: %v", err)
		case <-deadline:
			t.Fatal("engine did not become idle")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestAddValidation(t *testing.T) {
	e := startEngine(t, testConfig(t))

	if _, err := e.AddURL("ftp://example.com/file.bin", nil); err == nil {
		t.Error("expected error for non-HTTP scheme")
	}
	if _, err := e.AddURL("magnet:?xt=urn:btih:00000000000000000000", nil); err == nil {
		t.Error("expected error for magnet passed to AddURL")
	}
	if _, err := e.AddMagnet("http://example.com/file.bin", nil); err == nil {
		t.Error("expected error for HTTP URL passed to AddMagnet")
	}
	if _, err := e.AddTorrent([]byte("not a torrent"), nil); err != nil {
		t.Errorf("AddTorrent(invalid bytes) error = %v, want queued download that fails later", err)
	}
}

func TestStatusUnknownGID(t *testing.T) {
	e := startEngine(t, testConfig(t))
	if _, err := e.Status("00000000000000ff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Status(unknown) error = %v, want ErrNotFound", err)
	}
	if _, err := e.Files("00000000000000ff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Files(unknown) error = %v, want ErrNotFound", err)
	}
	if err := e.Cancel("00000000000000ff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel(unknown) error = %v, want ErrNotFound", err)
	}
}

func TestEventsStopDelivery(t *testing.T) {
	payload := []byte("telemirror-event-stop-payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()

	e := startEngine(t, testConfig(t))
	events, stop := e.Events()

	gid, err := e.AddURL(srv.URL+"/file.bin", nil)
	if err != nil {
		t.Fatalf("AddURL() error = %v", err)
	}
	waitForEvent(t, events, gid, EventComplete, 15*time.Second)

	stop()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Error("events channel not closed after stop")
			return
		}
	}
}
