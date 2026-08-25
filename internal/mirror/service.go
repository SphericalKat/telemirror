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
	// AddURL queues an HTTP or HTTPS download in opts.Dir and returns its GID.
	AddURL(rawURL string, opts *engine.AddOptions) (string, error)

	// Status returns the current snapshot of one download.
	Status(gid string) (engine.DownloadInfo, error)

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
}

// Message lifetimes follow the upstream bot: temporary replies disappear
// after ten seconds, and status messages disappear ten seconds after the
// last tracked download finishes.
const (
	temporaryReplyDeleteDelay = 10 * time.Second
	statusDeleteDelay         = 10 * time.Second
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
	repliedUsername string
	started         time.Time

	mu              sync.Mutex
	uploading       bool
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
	case "mirror":
		if !s.isAuthorized(msg) {
			s.sendTemporaryReply(ctx, msg, "You aren't authorized to use this bot here.")
			return
		}
		s.handleMirror(ctx, msg, arg)
	default:
		// Other commands are handled by later feature work.
	}
}

// handleMirror accepts an HTTP(S) mirror request.
func (s *Service) handleMirror(ctx context.Context, msg *telegram.Message, url string) {
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
		repliedUsername: renderedUsername(msg.ReplyToMessage),
		started:         time.Now(),
	}

	// Hold the record lock while the download is added, so an event that
	// arrives during AddURL cannot run before the record exists.
	s.recMu.Lock()
	gid, err := s.dl.AddURL(url, &engine.AddOptions{Dir: rec.dir})
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
		log.Printf("mirror: gid %s started. Dir %s", ev.GID, rec.dir)
		s.refreshStatuses()
	case engine.EventComplete:
		go s.handleComplete(ctx, rec)
	case engine.EventError:
		go s.handleFailure(ctx, rec)
	case engine.EventStop:
		go s.finish(ctx, rec, "Download stopped.")
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
	if len(info.FollowedBy) > 0 || len(info.Files) == 0 || info.Files[0].Path == "" {
		// Torrent metadata and fileless completions carry nothing to publish.
		s.finish(ctx, rec, "Upload failed. Could not get files.")
		return
	}

	root := downloadRoot(info)
	name := filepath.Base(root)
	size := info.TotalLength

	rec.setUploading(true)
	s.refreshStatuses()

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
func (s *Service) finish(ctx context.Context, rec *record, message string) {
	if rec.repliedUsername != "" {
		message += fmt.Sprintf("\ncc: %s", rec.repliedUsername)
	}

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

	if _, err := s.tg.SendMessage(context.WithoutCancel(ctx), rec.chatID, message, rec.messageID); err != nil {
		log.Printf("mirror: send completion reply: %v", err)
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
	for _, id := range s.cfg.SudoUsers {
		if id == msg.From.ID {
			return true
		}
	}
	for _, id := range s.cfg.AuthorizedChats {
		if id == msg.Chat.ID {
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

// sendTemporaryReply replies to a command message and removes both messages
// after the temporary reply lifetime.
func (s *Service) sendTemporaryReply(ctx context.Context, msg *telegram.Message, text string) {
	sent, err := s.tg.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID)
	if err != nil {
		log.Printf("mirror: send reply: %v", err)
		return
	}
	s.scheduleDelete(msg.Chat.ID, sent.MessageID, temporaryReplyDeleteDelay)
	s.scheduleDelete(msg.Chat.ID, msg.MessageID, temporaryReplyDeleteDelay)
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
