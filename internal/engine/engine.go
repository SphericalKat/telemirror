// Package engine embeds the forked aria2go library as Telemirror's
// in-process download engine. It adds downloads, reports per-download
// status, cancels downloads, delivers lifecycle events, and ties the
// engine lifetime to the application context. No aria2c process, child
// process, or external RPC is used.
package engine

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	aria2c "github.com/smartass08/aria2go/pkg/aria2go"
)

// ErrNotFound reports a GID the engine no longer knows, for example a
// queued download that was cancelled. Use errors.Is to detect it.
var ErrNotFound = aria2c.ErrDownloadNotFound

// Engine wraps the forked aria2go daemon for in-process use.
type Engine struct {
	daemon *aria2c.Daemon
	cfg    Config
}

// New creates an engine. Call Run to start it; downloads added before Run
// wait in the queue.
func New(cfg Config) (*Engine, error) {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	// SeedTime "0" stops BitTorrent downloads as soon as they complete,
	// matching the upstream bot's aria2 configuration. KeepRunning keeps
	// the engine alive between downloads until the context stops it.
	opts := &aria2c.EngineOptions{
		Dir:                    cfg.DownloadDir,
		MaxConcurrentDownloads: maxConcurrent,
		SeedTime:               "0",
		KeepRunning:            true,
	}
	if cfg.CACertPath != "" {
		opts.CACertificate = cfg.CACertPath
	}

	daemon, err := aria2c.New(aria2c.Config{Engine: opts})
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	return &Engine{daemon: daemon, cfg: cfg}, nil
}

// Run starts the engine and blocks until ctx is cancelled. Active
// downloads are halted and recorded before Run returns.
func (e *Engine) Run(ctx context.Context) error {
	return e.daemon.Run(ctx)
}

func (e *Engine) addOptions(opts *AddOptions) *aria2c.DownloadOptions {
	if opts == nil {
		return nil
	}
	out := &aria2c.DownloadOptions{}
	if opts.Dir != "" {
		out.Dir = opts.Dir
	}
	if opts.Out != "" {
		out.Out = opts.Out
	}
	return out
}

// AddURL adds an HTTP or HTTPS download. A URL that names a .torrent file
// or answers with the BitTorrent content type downloads the torrent
// metadata and enqueues the described torrent as a followed child
// download; the completion event for the metadata GID carries the child
// GID in FollowedBy.
func (e *Engine) AddURL(rawURL string, opts *AddOptions) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("engine: invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("engine: AddURL supports HTTP and HTTPS, got %q", scheme)
	}

	gid, err := e.daemon.AddURI(rawURL, e.addOptions(opts))
	if err != nil {
		return "", fmt.Errorf("engine: add URL: %w", err)
	}
	return gid.Hex(), nil
}

// AddMagnet adds a BitTorrent magnet download. The returned GID tracks the
// metadata download; its completion event carries the GID of the child
// download that holds the real files in FollowedBy.
func (e *Engine) AddMagnet(magnetURI string, opts *AddOptions) (string, error) {
	if !strings.HasPrefix(magnetURI, "magnet:?") {
		return "", fmt.Errorf("engine: AddMagnet supports magnet links, got %q", magnetURI)
	}

	gid, err := e.daemon.AddURI(magnetURI, e.addOptions(opts))
	if err != nil {
		return "", fmt.Errorf("engine: add magnet: %w", err)
	}
	return gid.Hex(), nil
}

// AddTorrent adds a download described by raw bencoded torrent data.
func (e *Engine) AddTorrent(data []byte, opts *AddOptions) (string, error) {
	gid, err := e.daemon.AddTorrent(data, e.addOptions(opts))
	if err != nil {
		return "", fmt.Errorf("engine: add torrent: %w", err)
	}
	return gid.Hex(), nil
}

func (e *Engine) parseGID(gid string) (aria2c.GID, error) {
	parsed, err := aria2c.ParseGID(gid)
	if err != nil {
		return 0, fmt.Errorf("engine: invalid GID %q: %w", gid, err)
	}
	return parsed, nil
}

// Status returns the per-download snapshot for gid.
func (e *Engine) Status(gid string) (DownloadInfo, error) {
	parsed, err := e.parseGID(gid)
	if err != nil {
		return DownloadInfo{}, err
	}
	st, err := e.daemon.TellStatus(parsed)
	if err != nil {
		return DownloadInfo{}, err
	}
	return toDownloadInfo(st), nil
}

// Files returns the file list for gid.
func (e *Engine) Files(gid string) ([]File, error) {
	info, err := e.Status(gid)
	if err != nil {
		return nil, err
	}
	return info.Files, nil
}

// Size returns the total download size in bytes for gid.
func (e *Engine) Size(gid string) (int64, error) {
	info, err := e.Status(gid)
	if err != nil {
		return 0, err
	}
	return info.TotalLength, nil
}

// Cancel removes a queued or active download. A queued download disappears
// without a result entry; an active download is halted and recorded with
// the removed status. Cancelling a finished or unknown download returns an
// error that matches ErrNotFound.
func (e *Engine) Cancel(gid string) error {
	parsed, err := e.parseGID(gid)
	if err != nil {
		return err
	}
	if err := e.daemon.Cancel(parsed); err != nil {
		return err
	}
	return nil
}

// Stats returns aggregate queue counters: active, waiting, and stopped
// download counts.
func (e *Engine) Stats() (active, waiting, stopped int) {
	st := e.daemon.Status()
	return st.Active, st.Waiting, st.Stopped
}

// Events returns a channel with lifecycle events for every download. The
// returned function stops delivery and closes the channel; it is safe to
// call more than once. Keep draining the channel while downloads run,
// because slow consumers miss events.
func (e *Engine) Events() (<-chan Event, func()) {
	src, stopSource := e.daemon.Subscribe()

	out := make(chan Event, 256)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case ev := <-src:
				event := Event{
					GID:  ev.GID.Hex(),
					Type: mapEventKind(ev.Kind),
				}
				if ev.Kind == aria2c.EventComplete {
					event.FollowedBy = e.followedBy(ev.GID)
				}
				select {
				case out <- event:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	return out, func() {
		once.Do(func() {
			stopSource()
			close(done)
		})
	}
}

func (e *Engine) followedBy(gid aria2c.GID) []string {
	st, err := e.daemon.TellStatus(gid)
	if err != nil {
		return nil
	}
	if len(st.FollowedBy) == 0 {
		return nil
	}
	gids := make([]string, 0, len(st.FollowedBy))
	for _, g := range st.FollowedBy {
		gids = append(gids, g.Hex())
	}
	return gids
}

func mapEventKind(kind aria2c.EventKind) EventType {
	switch kind {
	case aria2c.EventStart:
		return EventStart
	case aria2c.EventPause:
		return EventPause
	case aria2c.EventStop:
		return EventStop
	case aria2c.EventComplete:
		return EventComplete
	case aria2c.EventError:
		return EventError
	case aria2c.EventBTComplete:
		return EventBTComplete
	default:
		return EventType(kind)
	}
}

func toDownloadInfo(st aria2c.DownloadStatus) DownloadInfo {
	files := make([]File, 0, len(st.Files))
	for _, f := range st.Files {
		files = append(files, File{
			Index:           f.Index,
			Path:            f.Path,
			Length:          f.Length,
			CompletedLength: f.CompletedLength,
			Selected:        f.Selected,
		})
	}
	var followedBy []string
	for _, g := range st.FollowedBy {
		followedBy = append(followedBy, g.Hex())
	}
	following := ""
	if st.Following != 0 {
		following = st.Following.Hex()
	}
	return DownloadInfo{
		GID:             st.GID.Hex(),
		Status:          Status(st.Status),
		TotalLength:     st.TotalLength,
		CompletedLength: st.CompletedLength,
		DownloadSpeed:   st.DownloadSpeed,
		UploadSpeed:     st.UploadSpeed,
		Files:           files,
		ErrorMessage:    st.ErrorMessage,
		FollowedBy:      followedBy,
		Following:       following,
		InfoHash:        st.InfoHash,
		Dir:             st.Dir,
	}
}
