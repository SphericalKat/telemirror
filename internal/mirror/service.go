// Package mirror implements the central mirror service.
//
// The service accepts Telegram commands, tracks mirror requests, drives the
// embedded download engine, publishes completed downloads to Google Drive,
// and reports every step through status and reply messages. Telegram, the
// downloader, and the Drive publisher are replaceable boundaries, so tests
// can exercise complete workflows with fakes.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/telegram"
)

// Downloader is the download engine boundary the service needs.
// The embedded engine implements it.
type Downloader interface {
	// AddURL queues an HTTP or HTTPS download in opts.Dir and returns its
	// GID. A URL that names a .torrent file or answers with the BitTorrent
	// content type first downloads the torrent metadata; the completion
	// event then carries the GID of the download that holds the real files
	// in FollowedBy.
	AddURL(rawURL string, opts *engine.AddOptions) (string, error)

	// Cancel removes a queued or active download. A queued download
	// disappears without a stop event; an active download is stopped and
	// reports one.
	Cancel(gid string) error
	// AddMagnet queues a BitTorrent magnet download in opts.Dir and
	// returns the GID of the metadata download. Its completion event
	// carries the GID of the download that holds the real files in
	// FollowedBy.
	AddMagnet(magnetURI string, opts *engine.AddOptions) (string, error)

	// Status returns the current snapshot of one download.
	Status(gid string) (engine.DownloadInfo, error)

	// Cancel removes a queued or active download. An unknown GID reports
	// an error that matches engine.ErrNotFound.
	Cancel(gid string) error

	// Events returns the lifecycle event stream for all downloads.
	Events() (<-chan engine.Event, func())
}

// Publisher publishes a downloaded file or directory to Drive.
// The Drive publisher implements it.
type Publisher interface {
	Publish(ctx context.Context, root string, onProgress func(drive.Progress)) (drive.Result, error)
}

// Config holds the settings the mirror service needs.
type Config struct {
	// SudoUsers lists the Telegram users who can use the bot in any chat.
	SudoUsers []int64

	// AuthorizedChats lists the Telegram chats where all users can use the bot.
	AuthorizedChats []int64

	// DownloadDir is the base directory that holds one directory per request.
	DownloadDir string

	// FilteredDomains lists the blocked domain substrings.
	FilteredDomains []string

	// FilteredFilenames lists the blocked file-name substrings. The filter
	// applies with case-sensitive substring matching once the real file
	// name of a download becomes known.
	FilteredFilenames []string

	// StatusUpdateInterval is the delay between two status message updates.
	StatusUpdateInterval time.Duration

	// CommandsUseBotName makes group commands require the bot username.
	CommandsUseBotName bool

	// CommandBotName is the bot username that group commands require,
	// with or without the leading @.
	CommandBotName string

	// IsTeamDrive publishes results to a Shared Drive and adds the upstream
	// folder-link limitation notice to folder results.
	IsTeamDrive bool

	// StatusMessageTTL is how long a status message created by
	// /mirrorStatus lives before the bot removes it. Zero means the
	// upstream lifetime of 60 seconds.
	StatusMessageTTL time.Duration

	// TemporaryReplyDeleteDelay is how long a temporary reply and its
	// command live before the bot removes them. Zero means the upstream
	// lifetime of 10 seconds.
	TemporaryReplyDeleteDelay time.Duration
}

// Message lifetimes follow the upstream bot: temporary replies disappear
// after ten seconds, a status message created by /mirrorStatus disappears
// after sixty seconds, and the final status messages disappear ten seconds
// after the last tracked download finishes.
const (
	temporaryReplyDeleteDelay = 10 * time.Second
	statusDeleteDelay         = 10 * time.Second
	defaultStatusMessageTTL   = 60 * time.Second
)

// unauthorizedMessage is the upstream response for commands from senders
// who may not use the bot.
const unauthorizedMessage = "You aren't authorized to use this bot here."

// Upstream access codes, ordered from strongest to weakest control.
const (
	authSudo       = 0 // A configured sudo user.
	authOwner      = 1 // The download owner replying to the request.
	authChatAdmins = 2 // A member of an authorized chat where all members administrate.
	authChatMember = 3 // A member of an authorized chat with an unknown role.
	authDenied     = -1
)

// Service is the central mirror service.
type Service struct {
	cfg Config
	tg  telegram.Client
	dl  Downloader
	pub Publisher

	// recMu guards the records and statuses maps and the record identity.
	recMu sync.Mutex
	// records maps a download GID to its mirror request.
	records map[string]*record
	// statuses maps a chat ID to its single status message.
	statuses map[int64]*statusMessage

	// statusMu serializes status message sends, edits, and deletions.
	statusMu sync.Mutex
}

// record tracks one accepted mirror request.
// The fixed fields never change after creation; the mu-guarded fields track
// download and upload progress.
type record struct {
	gid string
	url string
	// dir is the absolute path of the request's unique download directory.
	dir             string
	chatID          int64
	messageID       int64
	userID          int64
	username        string
	repliedUsername string
	started         time.Time
	// tar marks a /mirrorTar request whose directory result is archived
	// before publication.
	tar bool

	mu              sync.Mutex
	downloadStarted bool
	uploading       bool
	cancelNotified  bool
	blocked         bool
	uploadedBytes   int64
	lastUploadBytes int64
	lastUploadCheck time.Time
}

// statusView is a concurrency-safe snapshot of the fields a status line
// renders.
type statusView struct {
	url           string
	uploading     bool
	uploadedBytes int64
	speed         int64
}

// setUploading marks the record as uploading or not.
func (r *record) setUploading(uploading bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uploading = uploading
}

// markStarted records that the engine started the download.
func (r *record) markStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.downloadStarted = true
}

// hasStarted reports whether the engine ever started the download.
// A download that was still queued reports no stop event when cancelled.
func (r *record) hasStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.downloadStarted
}

// isUploading reports whether the record's result is being uploaded.
func (r *record) isUploading() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uploading
}

// markCancelNotified records that the chat was notified about a manual
// cancellation, so the final reply for the request is suppressed.
func (r *record) markCancelNotified() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelNotified = true
}

// cancelNotifiedChat reports whether the chat was notified about a manual
// cancellation of this request.
func (r *record) cancelNotifiedChat() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelNotified
}

// setBlocked marks the record's download as stopped by the file-name filter.
func (r *record) setBlocked() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocked = true
}

// isBlocked reports whether the file-name filter stopped this download.
func (r *record) isBlocked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blocked
}

// setUploadedBytes records upload progress.
func (r *record) setUploadedBytes(sent int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uploadedBytes = sent
}

// view returns the rendering snapshot and updates the upload speed tracker.
func (r *record) view() statusView {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var speed int64
	if !r.lastUploadCheck.IsZero() {
		if elapsed := now.Sub(r.lastUploadCheck).Seconds(); elapsed > 0 {
			speed = int64(float64(r.uploadedBytes-r.lastUploadBytes) / elapsed)
		}
	}
	r.lastUploadBytes = r.uploadedBytes
	r.lastUploadCheck = now
	return statusView{
		url:           r.url,
		uploading:     r.uploading,
		uploadedBytes: r.uploadedBytes,
		speed:         speed,
	}
}

// statusMessage is the single status message one chat holds.
type statusMessage struct {
	chatID    int64
	messageID int64
	lastText  string
}

// New creates a mirror service. Start the service with Run before handling
// updates, because Run subscribes to the download events.
func New(cfg Config, tg telegram.Client, dl Downloader, pub Publisher) (*Service, error) {
	switch {
	case tg == nil:
		return nil, errors.New("mirror service requires a Telegram client")
	case dl == nil:
		return nil, errors.New("mirror service requires a downloader")
	case pub == nil:
		return nil, errors.New("mirror service requires a publisher")
	case strings.TrimSpace(cfg.DownloadDir) == "":
		return nil, errors.New("mirror service requires a download directory")
	case cfg.StatusUpdateInterval <= 0:
		return nil, errors.New("mirror service requires a positive status update interval")
	case cfg.CommandsUseBotName && strings.TrimSpace(cfg.CommandBotName) == "":
		return nil, errors.New("mirror service requires a bot name when commands must use it")
	}
	return &Service{
		cfg:      cfg,
		tg:       tg,
		dl:       dl,
		pub:      pub,
		records:  map[string]*record{},
		statuses: map[int64]*statusMessage{},
	}, nil
}

// Run consumes download events and refreshes status messages until ctx is
// cancelled or the event stream closes. Run blocks; call it once.
func (s *Service) Run(ctx context.Context) error {
	events, stop := s.dl.Events()
	defer stop()

	ticker := time.NewTicker(s.cfg.StatusUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			s.handleEvent(ctx, ev)
		case <-ticker.C:
			if s.trackedCount() > 0 {
				s.enforceFilenameFilters(ctx)
				s.refreshStatuses()
			}
		}
	}
}

// HandleUpdate processes one Telegram update.
func (s *Service) HandleUpdate(ctx context.Context, upd telegram.Update) {
	msg := upd.Message
	if msg == nil || msg.From == nil || !strings.HasPrefix(msg.Text, "/") {
		return
	}
	command, suffix, arg := parseCommand(msg.Text)

	// Group chats may require the configured bot username after a command.
	// Command matching is case-insensitive, like the upstream bot.
	if s.cfg.CommandsUseBotName && msg.Chat.Type != "private" && !strings.EqualFold(suffix, s.requiredBotName()) {
		return
	}

	switch command {
	case "mirror", "mirrortar":
		if !s.isAuthorized(msg) {
			s.sendTemporaryReply(ctx, msg, unauthorizedMessage)
			return
		}
		s.handleMirror(ctx, msg, arg, command == "mirrortar")
	case "start":
		// The upstream command set accepts no argument for these commands.
		if arg != "" {
			return
		}
		if !s.isAuthorized(msg) {
			s.sendTemporaryReply(ctx, msg, unauthorizedMessage)
			return
		}
		s.sendPermanentReply(ctx, msg, "You should know the commands already. Happy mirroring.")
	case "mirrorstatus":
		if arg != "" {
			return
		}
		if !s.isAuthorized(msg) {
			s.sendTemporaryReply(ctx, msg, unauthorizedMessage)
			return
		}
		s.handleMirrorStatus(ctx, msg)
	case "cancelmirror":
		if arg != "" {
			return
		}
		s.handleCancelMirror(ctx, msg)
	case "cancelall":
		if arg != "" {
			return
		}
		s.handleCancelAll(ctx, msg)
	default:
		// Other commands are handled by later feature work.
	}
}

// handleMirror accepts a mirror request. The input is an HTTP(S) URL, a URL
// for a remote torrent file, or a BitTorrent magnet link. A tar request
// archives a directory result before publication.
func (s *Service) handleMirror(ctx context.Context, msg *telegram.Message, url string, tar bool) {
	if url == "" {
		// The upstream bot ignores a mirror command without a URL.
		return
	}
	if !s.isDownloadAllowed(url) {
		s.sendTemporaryReply(ctx, msg, "Download failed. Blacklisted URL.")
		return
	}

	rec := &record{
		url:             url,
		dir:             filepath.Join(s.cfg.DownloadDir, uuid.NewString()),
		chatID:          msg.Chat.ID,
		messageID:       msg.MessageID,
		userID:          msg.From.ID,
		username:        renderedUsername(msg),
		repliedUsername: renderedUsername(msg.ReplyToMessage),
		started:         time.Now(),
		tar:             tar,
	}

	// Hold the record lock while the download is added, so an event that
	// arrives during AddURL cannot run before the record exists.
	s.recMu.Lock()
	var gid string
	var err error
	if isMagnetURI(url) {
		gid, err = s.dl.AddMagnet(url, &engine.AddOptions{Dir: rec.dir})
	} else {
		gid, err = s.dl.AddURL(url, &engine.AddOptions{Dir: rec.dir})
	}
	if err != nil {
		s.recMu.Unlock()
		message := fmt.Sprintf("Failed to start the download. %v", err)
		log.Printf("mirror: %s", message)
		s.finish(ctx, rec, message)
		return
	}
	rec.gid = gid
	s.records[gid] = rec
	s.recMu.Unlock()

	log.Printf("mirror: gid %s download %s", gid, url)
	s.sendStatusMessage(context.WithoutCancel(ctx), rec)
}

// handleMirrorStatus answers /mirrorStatus: it replaces the chat's status
// message with a fresh one, removes the command at once, and removes the
// status message after its lifetime, like the upstream bot.
func (s *Service) handleMirrorStatus(ctx context.Context, msg *telegram.Message) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	if old, ok := s.statuses[msg.Chat.ID]; ok {
		delete(s.statuses, msg.Chat.ID)
		if err := s.tg.DeleteMessage(ctx, msg.Chat.ID, old.messageID); err != nil {
			log.Printf("mirror: delete old status message: %v", err)
		}
	}

	text := s.statusText()
	sent, err := s.tg.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID)
	if err != nil {
		log.Printf("mirror: send status message: %v", err)
		return
	}
	s.recMu.Lock()
	s.statuses[msg.Chat.ID] = &statusMessage{chatID: msg.Chat.ID, messageID: sent.MessageID, lastText: text}
	s.recMu.Unlock()

	if err := s.tg.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID); err != nil {
		log.Printf("mirror: delete status command: %v", err)
	}

	chatID, messageID := msg.Chat.ID, sent.MessageID
	time.AfterFunc(s.statusTTL(), func() {
		s.expireStatusMessage(chatID, messageID)
	})
}

// expireStatusMessage removes one status message whose lifetime ended. It
// does nothing when the chat already holds a newer status message.
func (s *Service) expireStatusMessage(chatID, messageID int64) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.recMu.Lock()
	current, ok := s.statuses[chatID]
	if ok && current.messageID == messageID {
		delete(s.statuses, chatID)
	}
	s.recMu.Unlock()
	if !ok || current.messageID != messageID {
		return
	}
	if err := s.tg.DeleteMessage(context.Background(), chatID, messageID); err != nil {
		log.Printf("mirror: delete status message: %v", err)
	}
}

// handleCancelMirror answers /cancelMirror. The sender must reply to the
// original command message of the mirror request being cancelled.
func (s *Service) handleCancelMirror(ctx context.Context, msg *telegram.Message) {
	if msg.ReplyToMessage == nil {
		s.sendTemporaryReply(ctx, msg, "Reply to the command message for the download that you want to cancel.")
		return
	}
	rec := s.findByMessage(msg.ReplyToMessage)
	if rec == nil {
		s.sendTemporaryReply(ctx, msg, "Reply to the command message for the download that you want to cancel."+
			" Also make sure that the download is even active.")
		return
	}

	switch code := s.authorization(msg, false); {
	case code > authDenied && code < authChatMember:
		s.cancelDownload(ctx, msg, rec)
	case code == authChatMember:
		if s.isAdmin(ctx, msg) {
			s.cancelDownload(ctx, msg, rec)
		} else {
			s.sendTemporaryReply(ctx, msg, "You do not have permission to do that.")
		}
	default:
		s.sendTemporaryReply(ctx, msg, unauthorizedMessage)
	}
}

// handleCancelAll answers /cancelAll. A sudo user cancels every mirror
// request; a chat administrator cancels the requests of that chat only.
func (s *Service) handleCancelAll(ctx context.Context, msg *telegram.Message) {
	code := s.authorization(msg, true)
	if code == authDenied {
		s.sendTemporaryReply(ctx, msg, unauthorizedMessage)
		return
	}
	if code == authChatMember && !s.isAdmin(ctx, msg) {
		s.sendTemporaryReply(ctx, msg, "You do not have permission to do that.")
		return
	}

	s.recMu.Lock()
	targets := make([]*record, 0, len(s.records))
	for _, rec := range s.records {
		if code == authSudo || rec.chatID == msg.Chat.ID {
			targets = append(targets, rec)
		}
	}
	s.recMu.Unlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].started.Before(targets[j].started) })

	// Mark every target before stopping it, so a stop event that arrives
	// while the loop runs already sees the suppression, and collect the
	// distinct owner names of each origin chat for one notice per chat.
	usernames := map[int64][]string{}
	for _, rec := range targets {
		rec.markCancelNotified()
		if !slices.Contains(usernames[rec.chatID], rec.username) {
			usernames[rec.chatID] = append(usernames[rec.chatID], rec.username)
		}
	}

	count := 0
	for _, rec := range targets {
		if s.cancelDownload(ctx, nil, rec) {
			count++
		}
	}

	if count == 0 {
		s.sendTemporaryReply(ctx, msg, "No downloads to cancel")
		return
	}
	s.sendPermanentReply(ctx, msg, fmt.Sprintf("%d downloads cancelled.", count))

	chats := make([]int64, 0, len(usernames))
	for chatID := range usernames {
		chats = append(chats, chatID)
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
	for _, chatID := range chats {
		message := strings.Join(usernames[chatID], ", ") + ", your downloads have been manually cancelled."
		if _, err := s.tg.SendMessage(ctx, chatID, message, 0); err != nil {
			log.Printf("mirror: send cancellation notice: %v", err)
		}
	}
}

// cancelDownload stops one mirror request and reports whether it was
// cancelled. An uploading request is rejected, like the upstream bot. A
// nil cancelMsg suppresses the per-command replies, which /cancelAll uses.
func (s *Service) cancelDownload(ctx context.Context, msg *telegram.Message, rec *record) bool {
	if rec.isUploading() {
		if msg != nil {
			s.sendTemporaryReply(ctx, msg, "Upload in progress. Cannot cancel.")
		}
		return false
	}

	if err := s.dl.Cancel(rec.gid); err != nil {
		log.Printf("mirror: cancel gid %s: %v", rec.gid, err)
	}
	if msg != nil && rec.chatID != msg.Chat.ID {
		// Notify when the cancellation happened outside the origin chat.
		s.sendTemporaryReply(ctx, msg, "The download was canceled.")
	}
	if !rec.hasStarted() {
		// The engine reports no stop event for a download that never
		// started, so finish the request here, like the upstream bot.
		s.finish(ctx, rec, "Download stopped.")
	}
	return true
}

// handleEvent dispatches one download lifecycle event.
func (s *Service) handleEvent(ctx context.Context, ev engine.Event) {
	s.recMu.Lock()
	rec, ok := s.records[ev.GID]
	s.recMu.Unlock()
	if !ok {
		// Events for downloads without a mirror request carry no work.
		return
	}

	switch ev.Type {
	case engine.EventStart:
		rec.markStarted()
		log.Printf("mirror: gid %s started. Dir %s", ev.GID, rec.dir)
		s.enforceFilenameFilters(ctx)
		s.refreshStatuses()
	case engine.EventComplete:
		go s.handleComplete(ctx, rec)
	case engine.EventError:
		go s.handleFailure(ctx, rec)
	case engine.EventStop:
		go s.finish(ctx, rec, stoppedMessage(rec))
	default:
		// Pause and BitTorrent events belong to later feature work.
	}
}

// handleComplete publishes a completed download and replies with the result.
func (s *Service) handleComplete(ctx context.Context, rec *record) {
	info, err := s.dl.Status(rec.gid)
	if err != nil {
		log.Printf("mirror: gid %s complete: get status: %v", rec.gid, err)
		s.finish(ctx, rec, "Upload failed. Could not get downloaded files.")
		return
	}
	if len(info.FollowedBy) > 0 {
		// A torrent metadata download completed; the followed download
		// holds the real files and needs the record's attention.
		s.followDownload(ctx, rec, info.FollowedBy[0])
		return
	}
	if len(info.Files) == 0 || info.Files[0].Path == "" {
		// Fileless completions carry nothing to publish.
		s.finish(ctx, rec, "Upload failed. Could not get files.")
		return
	}

	root := downloadRoot(info)
	if strings.HasSuffix(root, ".torrent") {
		// The engine follows torrent metadata with a child download. A
		// completed metadata file without a child carries no publishable
		// result.
		s.finish(ctx, rec, "Upload failed. Could not get files.")
		return
	}
	name := filepath.Base(root)
	size := info.TotalLength

	rec.setUploading(true)
	s.refreshStatuses()

	if s.filenameDisallowed(name) {
		log.Printf("mirror: gid %s blacklisted. Filename %s", rec.gid, name)
		s.finish(ctx, rec, "Upload failed. Blacklisted file name.")
		return
	}

	if rec.tar {
		archivedPath, archivedSize, err := archiveResult(root)
		if err != nil {
			log.Printf("mirror: gid %s archive: %v", rec.gid, err)
			s.finish(ctx, rec, fmt.Sprintf("Failed to upload <code>%s.tar</code> to Drive. %v", name, err))
			return
		}
		if archivedPath != root {
			log.Printf("mirror: gid %s archived %s", rec.gid, archivedPath)
			root, name, size = archivedPath, name+".tar", archivedSize
		}
	}

	log.Printf("mirror: gid %s completed. Filename %s. Starting upload", rec.gid, name)
	result, err := s.pub.Publish(ctx, root, func(pr drive.Progress) {
		rec.setUploadedBytes(pr.UploadedBytes)
	})
	if err != nil {
		s.finish(ctx, rec, fmt.Sprintf("Failed to upload <code>%s</code> to Drive. %v", name, err))
		return
	}

	message := fmt.Sprintf("<a href='%s'>%s</a>", result.Link, name)
	if size > 0 {
		message = fmt.Sprintf("<a href='%s'>%s</a> (%s)", result.Link, name, formatSize(size))
	}
	if s.cfg.IsTeamDrive && result.IsFolder {
		message += "\n\n<i>Folders in Shared Drives can only be shared with members of the drive. Mirror as an archive if you need public links.</i>"
	}
	s.finish(ctx, rec, message)
}

// followDownload moves a record from a completed torrent metadata download
// onto the followed download that holds the real files. The followed download
// may already be complete while the metadata event was processed, so its
// status is checked to keep the result from being lost.
func (s *Service) followDownload(ctx context.Context, rec *record, childGID string) {
	log.Printf("mirror: gid %s changed to %s", rec.gid, childGID)

	s.recMu.Lock()
	if tracked, exists := s.records[childGID]; exists && tracked != rec {
		s.recMu.Unlock()
		return
	}
	delete(s.records, rec.gid)
	rec.gid = childGID
	s.records[childGID] = rec
	s.recMu.Unlock()

	child, err := s.dl.Status(childGID)
	if err != nil {
		// The followed download is not known yet; its events drive it.
		return
	}
	switch child.Status {
	case engine.StatusComplete:
		s.handleComplete(ctx, rec)
	case engine.StatusError:
		s.handleFailure(ctx, rec)
	case engine.StatusRemoved:
		s.finish(ctx, rec, stoppedMessage(rec))
	}
}

// stoppedMessage renders the reply for a stopped download. A download stopped
// by the file-name filter says so, like the upstream bot.
func stoppedMessage(rec *record) string {
	if rec.isBlocked() {
		return "Download stopped. Blacklisted file name."
	}
	return "Download stopped."
}

// handleFailure reports a failed download with the engine error details.
func (s *Service) handleFailure(ctx context.Context, rec *record) {
	message := "Failed to download."
	if info, err := s.dl.Status(rec.gid); err == nil && info.ErrorMessage != "" {
		message = fmt.Sprintf("Failed to download. %s", info.ErrorMessage)
	}
	log.Printf("mirror: gid %s failed. %s", rec.gid, message)
	s.finish(ctx, rec, message)
}

// finish sends the final reply for a mirror request, removes its record, and
// deletes the local download directory. It runs at most once per request.
// A request cancelled through /cancelAll sends no reply, because the origin
// chat already received the manual-cancellation notice.
func (s *Service) finish(ctx context.Context, rec *record, message string) {
	s.recMu.Lock()
	if rec.gid != "" {
		if _, tracked := s.records[rec.gid]; !tracked {
			s.recMu.Unlock()
			return
		}
		delete(s.records, rec.gid)
	}
	remaining := len(s.records)
	s.recMu.Unlock()

	if !rec.cancelNotifiedChat() {
		if rec.repliedUsername != "" {
			message += fmt.Sprintf("\ncc: %s", rec.repliedUsername)
		}
		if _, err := s.tg.SendMessage(context.WithoutCancel(ctx), rec.chatID, message, rec.messageID); err != nil {
			log.Printf("mirror: send completion reply: %v", err)
		}
	}

	if err := os.RemoveAll(rec.dir); err != nil {
		log.Printf("mirror: cleanup: failed to delete %s: %v", rec.dir, err)
	} else {
		log.Printf("mirror: cleanup: deleted %s", rec.dir)
	}

	if remaining == 0 {
		s.deleteAllStatuses()
	} else {
		s.refreshStatuses()
	}
}

// isAuthorized reports whether the sender may use normal commands: a sudo
// user anywhere, or any user in an authorized chat.
func (s *Service) isAuthorized(msg *telegram.Message) bool {
	return s.authorization(msg, true) > authDenied
}

// authorization returns the upstream access code for msg:
//
//	authSudo       the sender is a configured sudo user
//	authOwner      the sender replies to their own mirror request
//	authChatAdmins the chat is authorized and marks all members as
//	               administrators
//	authChatMember the chat is authorized and the sender's role is unknown
//	authDenied     none of the above
//
// skipOwner disables the download-owner rule, which /cancelAll does because
// an owner may not cancel every request.
func (s *Service) authorization(msg *telegram.Message, skipOwner bool) int {
	for _, id := range s.cfg.SudoUsers {
		if id == msg.From.ID {
			return authSudo
		}
	}
	if !skipOwner && msg.ReplyToMessage != nil {
		if rec := s.findByMessage(msg.ReplyToMessage); rec != nil && rec.userID == msg.From.ID {
			return authOwner
		}
	}
	for _, id := range s.cfg.AuthorizedChats {
		if id == msg.Chat.ID {
			if msg.Chat.AllMembersAreAdministrators {
				return authChatAdmins
			}
			return authChatMember
		}
	}
	return authDenied
}

// findByMessage returns the tracked mirror request that was started by the
// given command message, or nil.
func (s *Service) findByMessage(msg *telegram.Message) *record {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	for _, rec := range s.records {
		if rec.chatID == msg.Chat.ID && rec.messageID == msg.MessageID {
			return rec
		}
	}
	return nil
}

// isAdmin reports whether the sender administrates the chat of msg. A failed
// administrator lookup counts as a negative answer, like the upstream bot.
func (s *Service) isAdmin(ctx context.Context, msg *telegram.Message) bool {
	admins, err := s.tg.ChatAdministrators(ctx, msg.Chat.ID)
	if err != nil {
		log.Printf("mirror: get chat administrators: %v", err)
		return false
	}
	for _, admin := range admins {
		if admin.ID == msg.From.ID {
			return true
		}
	}
	return false
}

// isDownloadAllowed reports whether the URL passes the domain filter.
// The filter is a case-sensitive substring check, like the upstream bot.
func (s *Service) isDownloadAllowed(url string) bool {
	for _, filtered := range s.cfg.FilteredDomains {
		if strings.Contains(url, filtered) {
			return false
		}
	}
	return true
}

// filenameDisallowed reports whether name contains a blocked file-name
// substring. The filter is a case-sensitive substring check, like the
// upstream bot. An empty name or an undetermined torrent metadata name is
// never blocked, because the real file name is not known yet.
func (s *Service) filenameDisallowed(name string) bool {
	if name == "" || name == "Metadata" {
		return false
	}
	for _, filtered := range s.cfg.FilteredFilenames {
		if strings.Contains(name, filtered) {
			return true
		}
	}
	return false
}

// enforceFilenameFilters stops tracked downloads whose real file name became
// known and is blocked, like the upstream bot does on every status update.
// A download whose upload already started is left alone; the completion path
// prevents publication instead.
func (s *Service) enforceFilenameFilters(ctx context.Context) {
	if len(s.cfg.FilteredFilenames) == 0 {
		return
	}

	s.recMu.Lock()
	recs := make([]*record, 0, len(s.records))
	for _, rec := range s.records {
		recs = append(recs, rec)
	}
	s.recMu.Unlock()

	for _, rec := range recs {
		if rec.isUploading() || rec.isBlocked() {
			continue
		}
		info, err := s.dl.Status(rec.gid)
		if err != nil {
			continue
		}
		name := downloadName(info, rec.url)
		if !s.filenameDisallowed(name) {
			continue
		}
		log.Printf("mirror: gid %s blacklisted. Filename %s", rec.gid, name)
		rec.setBlocked()
		if cancelErr := s.dl.Cancel(rec.gid); cancelErr != nil && !errors.Is(cancelErr, engine.ErrNotFound) {
			log.Printf("mirror: cancel gid %s: %v", rec.gid, cancelErr)
		}
		// Finish directly: the engine may not report the removal, and a
		// late stop event finds no record and changes nothing.
		s.finish(ctx, rec, stoppedMessage(rec))
	}
}

// isMagnetURI reports whether an input is a BitTorrent magnet link. The
// magnet scheme is matched without case sensitivity, like aria2.
func isMagnetURI(input string) bool {
	return strings.HasPrefix(strings.ToLower(input), "magnet:")
}

// sendTemporaryReply replies to a command message and removes both messages
// after the temporary reply lifetime.
func (s *Service) sendTemporaryReply(ctx context.Context, msg *telegram.Message, text string) {
	sent, err := s.tg.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID)
	if err != nil {
		log.Printf("mirror: send reply: %v", err)
		return
	}
	delay := s.replyDeleteDelay()
	s.scheduleDelete(msg.Chat.ID, sent.MessageID, delay)
	s.scheduleDelete(msg.Chat.ID, msg.MessageID, delay)
}

// sendPermanentReply replies to a command message and removes nothing, like
// the upstream replies that stay in the chat.
func (s *Service) sendPermanentReply(ctx context.Context, msg *telegram.Message, text string) {
	if _, err := s.tg.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID); err != nil {
		log.Printf("mirror: send reply: %v", err)
	}
}

// statusTTL returns the lifetime of a status message created by
// /mirrorStatus.
func (s *Service) statusTTL() time.Duration {
	if s.cfg.StatusMessageTTL > 0 {
		return s.cfg.StatusMessageTTL
	}
	return defaultStatusMessageTTL
}

// replyDeleteDelay returns the lifetime of a temporary reply.
func (s *Service) replyDeleteDelay() time.Duration {
	if s.cfg.TemporaryReplyDeleteDelay > 0 {
		return s.cfg.TemporaryReplyDeleteDelay
	}
	return temporaryReplyDeleteDelay
}

// scheduleDelete removes one message after delay.
func (s *Service) scheduleDelete(chatID, messageID int64, delay time.Duration) {
	time.AfterFunc(delay, func() {
		if err := s.tg.DeleteMessage(context.Background(), chatID, messageID); err != nil {
			log.Printf("mirror: delete message: %v", err)
		}
	})
}

// requiredBotName normalizes the configured bot name to a command suffix.
func (s *Service) requiredBotName() string {
	name := s.cfg.CommandBotName
	if !strings.HasPrefix(name, "@") {
		name = "@" + name
	}
	return name
}

// trackedCount returns the number of tracked mirror requests.
func (s *Service) trackedCount() int {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	return len(s.records)
}

// sendStatusMessage sends the status message for one chat, replacing the
// chat's previous status message, and stores it for later updates.
func (s *Service) sendStatusMessage(ctx context.Context, rec *record) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	if old, ok := s.statuses[rec.chatID]; ok {
		delete(s.statuses, rec.chatID)
		if err := s.tg.DeleteMessage(ctx, rec.chatID, old.messageID); err != nil {
			log.Printf("mirror: delete old status message: %v", err)
		}
	}

	text := s.statusText()
	sent, err := s.tg.SendMessage(ctx, rec.chatID, text, rec.messageID)
	if err != nil {
		log.Printf("mirror: send status message: %v", err)
		return
	}
	s.recMu.Lock()
	s.statuses[rec.chatID] = &statusMessage{chatID: rec.chatID, messageID: sent.MessageID, lastText: text}
	s.recMu.Unlock()
}

// refreshStatuses edits every status message whose text changed.
func (s *Service) refreshStatuses() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	text := s.statusText()

	s.recMu.Lock()
	pending := make([]*statusMessage, 0, len(s.statuses))
	for _, st := range s.statuses {
		pending = append(pending, st)
	}
	s.recMu.Unlock()

	for _, st := range pending {
		if st.lastText == text {
			continue
		}
		if err := s.tg.EditMessageText(context.Background(), st.chatID, st.messageID, text); err != nil {
			log.Printf("mirror: edit status message: %v", err)
			// The message is gone; stop updating it.
			s.recMu.Lock()
			delete(s.statuses, st.chatID)
			s.recMu.Unlock()
			continue
		}
		s.recMu.Lock()
		if current, ok := s.statuses[st.chatID]; ok && current.messageID == st.messageID {
			current.lastText = text
		}
		s.recMu.Unlock()
	}
}

// deleteAllStatuses removes every status message after the status lifetime.
func (s *Service) deleteAllStatuses() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.recMu.Lock()
	pending := make([]*statusMessage, 0, len(s.statuses))
	for _, st := range s.statuses {
		pending = append(pending, st)
	}
	s.statuses = map[int64]*statusMessage{}
	s.recMu.Unlock()

	for _, st := range pending {
		s.scheduleDelete(st.chatID, st.messageID, statusDeleteDelay)
	}
}

// statusText renders the status block for all tracked mirror requests,
// ordered by start time, like the upstream bot.
func (s *Service) statusText() string {
	s.recMu.Lock()
	recs := make([]*record, 0, len(s.records))
	for _, rec := range s.records {
		recs = append(recs, rec)
	}
	s.recMu.Unlock()

	if len(recs) == 0 {
		return "No active or queued downloads"
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].started.Before(recs[j].started) })

	lines := make([]string, 0, len(recs))
	for _, rec := range recs {
		info, err := s.dl.Status(rec.gid)
		if err != nil {
			lines = append(lines, fmt.Sprintf("Error: %s - %v", rec.gid, err))
			continue
		}
		lines = append(lines, statusLine(rec.view(), info))
	}
	return strings.Join(lines, "\n\n")
}

// parseCommand splits a command message into the command name, the optional
// @username suffix, and the argument after the first space.
func parseCommand(text string) (command, suffix, arg string) {
	token := text
	if i := strings.Index(text, " "); i >= 0 {
		token = text[:i]
		arg = text[i+1:]
	}
	token = strings.TrimPrefix(token, "/")
	if i := strings.Index(token, "@"); i >= 0 {
		suffix = token[i:]
		token = token[:i]
	}
	return strings.ToLower(token), suffix, arg
}

// renderedUsername renders a user the way the upstream bot does: the @user
// name, or a profile link when the user has none.
func renderedUsername(msg *telegram.Message) string {
	if msg == nil || msg.From == nil {
		return ""
	}
	if msg.From.Username != "" {
		return "@" + msg.From.Username
	}
	return fmt.Sprintf("<a href=\"tg://user?id=%d\">%s</a>", msg.From.ID, msg.From.FirstName)
}
