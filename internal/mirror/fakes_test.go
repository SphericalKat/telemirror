package mirror_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/SphericalKat/telemirror/internal/disk"
	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/telegram"
)

// fakeTelegram records Telegram operations instead of calling Telegram.
type fakeTelegram struct {
	mu         sync.Mutex
	nextID     int64
	sent       []sentMessage
	edited     []editedMessage
	deleted    []deletedMessage
	sendErr    error
	admins     map[int64][]int64
	adminCalls []int64
}

type sentMessage struct {
	ChatID    int64
	MessageID int64
	Text      string
	ReplyTo   int64
}

type editedMessage struct {
	ChatID    int64
	MessageID int64
	Text      string
}

type deletedMessage struct {
	ChatID    int64
	MessageID int64
}

func (f *fakeTelegram) SendMessage(_ context.Context, chatID int64, text string, replyTo int64) (telegram.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return telegram.Message{}, f.sendErr
	}
	f.nextID++
	f.sent = append(f.sent, sentMessage{ChatID: chatID, MessageID: f.nextID, Text: text, ReplyTo: replyTo})
	return telegram.Message{MessageID: f.nextID, Chat: telegram.Chat{ID: chatID}}, nil
}

func (f *fakeTelegram) EditMessageText(_ context.Context, chatID, messageID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edited = append(f.edited, editedMessage{ChatID: chatID, MessageID: messageID, Text: text})
	return nil
}

func (f *fakeTelegram) DeleteMessage(_ context.Context, chatID, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, deletedMessage{ChatID: chatID, MessageID: messageID})
	return nil
}

// ChatAdministrators answers with the configured administrators of a chat.
func (f *fakeTelegram) ChatAdministrators(_ context.Context, chatID int64) ([]telegram.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adminCalls = append(f.adminCalls, chatID)
	users := make([]telegram.User, 0, len(f.admins[chatID]))
	for _, id := range f.admins[chatID] {
		users = append(users, telegram.User{ID: id})
	}
	return users, nil
}

// setAdmins configures the administrators of one chat.
func (f *fakeTelegram) setAdmins(chatID int64, userIDs ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admins == nil {
		f.admins = map[int64][]int64{}
	}
	f.admins[chatID] = userIDs
}

// administratorCalls returns the chats whose administrators were requested.
func (f *fakeTelegram) administratorCalls() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.adminCalls...)
}

func (f *fakeTelegram) sends() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

func (f *fakeTelegram) edits() []editedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]editedMessage(nil), f.edited...)
}

func (f *fakeTelegram) deletions() []deletedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deletedMessage(nil), f.deleted...)
}

// newMessageID reserves the next message ID for a message the test creates.
func (f *fakeTelegram) newMessageID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return f.nextID
}

// lastSend returns the newest sent message.
func (f *fakeTelegram) lastSend() (sentMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentMessage{}, false
	}
	return f.sent[len(f.sent)-1], true
}

// fakeDownloader replaces the embedded engine. Tests script per-GID status
// snapshots and push lifecycle events through a channel.
type fakeDownloader struct {
	mu         sync.Mutex
	adds       []fakeAdd
	addErr     error
	cancelErr  error
	cancels    []string
	gidSeq     int
	infos      map[string]engine.DownloadInfo
	statusErrs map[string]error
	events     chan engine.Event
}

type fakeAdd struct {
	// Kind is "url" for AddURL calls and "magnet" for AddMagnet calls.
	Kind string
	URL  string
	Dir  string
}

func newFakeDownloader() *fakeDownloader {
	return &fakeDownloader{
		infos:      map[string]engine.DownloadInfo{},
		statusErrs: map[string]error{},
		events:     make(chan engine.Event, 64),
	}
}

func (f *fakeDownloader) AddURL(rawURL string, opts *engine.AddOptions) (string, error) {
	return f.add("url", rawURL, opts)
}

func (f *fakeDownloader) AddMagnet(magnetURI string, opts *engine.AddOptions) (string, error) {
	return f.add("magnet", magnetURI, opts)
}

func (f *fakeDownloader) add(kind, rawURL string, opts *engine.AddOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	dir := ""
	if opts != nil {
		dir = opts.Dir
	}
	f.gidSeq++
	gid := fmt.Sprintf("gid%d", f.gidSeq)
	f.adds = append(f.adds, fakeAdd{Kind: kind, URL: rawURL, Dir: dir})
	f.infos[gid] = engine.DownloadInfo{
		GID:    gid,
		Status: engine.StatusWaiting,
		Dir:    dir,
	}
	return gid, nil
}

// Cancel removes a download, following the engine contract: an active
// download is halted, keeps the removed status, and reports a stop event;
// a queued download disappears without an event; an unknown GID reports
// engine.ErrNotFound.
func (f *fakeDownloader) Cancel(gid string) error {
	f.mu.Lock()
	if f.cancelErr != nil {
		err := f.cancelErr
		f.mu.Unlock()
		return err
	}
	info, ok := f.infos[gid]
	if !ok {
		f.mu.Unlock()
		return engine.ErrNotFound
	}
	f.cancels = append(f.cancels, gid)
	wasActive := info.Status == engine.StatusActive
	info.Status = engine.StatusRemoved
	f.infos[gid] = info
	f.mu.Unlock()
	if wasActive {
		f.emit(engine.Event{GID: gid, Type: engine.EventStop})
	}
	return nil
}

func (f *fakeDownloader) Status(gid string) (engine.DownloadInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statusErrs[gid]; err != nil {
		return engine.DownloadInfo{}, err
	}
	info, ok := f.infos[gid]
	if !ok {
		return engine.DownloadInfo{}, fmt.Errorf("unknown GID %s", gid)
	}
	return info, nil
}

func (f *fakeDownloader) Events() (<-chan engine.Event, func()) {
	return f.events, func() {
		defer func() {
			// The service stops once; a second close would panic.
			_ = recover()
		}()
		close(f.events)
	}
}

func (f *fakeDownloader) added() []fakeAdd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeAdd(nil), f.adds...)
}

// setAddErr makes the next AddURL call fail with err.
func (f *fakeDownloader) setAddErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addErr = err
}

// cancelled returns the GIDs the service cancelled.
func (f *fakeDownloader) cancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancels...)
}

// cancelledGIDs returns the GIDs the service cancelled.
func (f *fakeDownloader) cancelledGIDs() []string {
	return f.cancelled()
}

func (f *fakeDownloader) setStatus(gid string, info engine.DownloadInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info.GID = gid
	f.infos[gid] = info
}

func (f *fakeDownloader) emit(ev engine.Event) {
	f.events <- ev
}

// fakeDiskUsage replaces the disk reporter. It records every requested
// path and answers with one scripted result.
type fakeDiskUsage struct {
	mu    sync.Mutex
	calls []string
	space disk.Space
	err   error
}

func newFakeDiskUsage() *fakeDiskUsage {
	return &fakeDiskUsage{space: disk.Space{
		TotalBytes: 500 * 1024 * 1024 * 1024,
		UsedBytes:  120 * 1024 * 1024 * 1024,
		FreeBytes:  380 * 1024 * 1024 * 1024,
	}}
}

func (f *fakeDiskUsage) Usage(path string) (disk.Space, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	return f.space, f.err
}

// requested returns the paths whose space was requested.
func (f *fakeDiskUsage) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// setErr makes later Usage calls fail with err.
func (f *fakeDiskUsage) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeLister replaces the Drive lister. Tests script results and errors and
// record the searched names.
type fakeLister struct {
	mu         sync.Mutex
	searches   []string
	results    []drive.Child
	err        error
	folderLink string
}

func newFakeLister() *fakeLister {
	return &fakeLister{folderLink: "https://drive.example/folder"}
}

func (f *fakeLister) List(_ context.Context, fileName string) ([]drive.Child, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, fileName)
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

func (f *fakeLister) FolderLink() string {
	return f.folderLink
}

func (f *fakeLister) searched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.searches...)
}

func (f *fakeLister) setResults(children []drive.Child) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = children
}

func (f *fakeLister) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakePublisher replaces the Drive publisher.
type fakePublisher struct {
	mu       sync.Mutex
	calls    []string
	result   drive.Result
	err      error
	block    chan struct{}
	called   chan struct{}
	callOnce sync.Once
}

func newFakePublisher(result drive.Result) *fakePublisher {
	return &fakePublisher{result: result, called: make(chan struct{})}
}

func (f *fakePublisher) Publish(_ context.Context, root string, onProgress func(drive.Progress)) (drive.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, root)
	block := f.block
	result, err := f.result, f.err
	f.mu.Unlock()
	f.callOnce.Do(func() { close(f.called) })
	if block != nil {
		<-block
	}
	if onProgress != nil && result.DriveID != "" {
		onProgress(drive.Progress{UploadedBytes: 1, TotalBytes: 2})
	}
	return result, err
}

func (f *fakePublisher) published() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// setBlock makes Publish calls wait until unblock.
func (f *fakePublisher) setBlock() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = make(chan struct{})
}

// unblock releases a blocked Publish call.
func (f *fakePublisher) unblock() {
	f.mu.Lock()
	block := f.block
	f.block = nil
	f.mu.Unlock()
	if block != nil {
		close(block)
	}
}

// setResult changes the result returned by later Publish calls.
func (f *fakePublisher) setResult(res drive.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result = res
}
