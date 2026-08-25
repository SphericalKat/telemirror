package engine

// Status describes the per-download state.
// The values mirror aria2 status strings.
type Status string

const (
	StatusWaiting  Status = "waiting"
	StatusActive   Status = "active"
	StatusPaused   Status = "paused"
	StatusComplete Status = "complete"
	StatusError    Status = "error"
	StatusRemoved  Status = "removed"
)

// EventType describes a lifecycle event emitted by the engine.
type EventType string

const (
	EventStart      EventType = "start"
	EventPause      EventType = "pause"
	EventStop       EventType = "stop"
	EventComplete   EventType = "complete"
	EventError      EventType = "error"
	EventBTComplete EventType = "btComplete"
)

// Event is a lifecycle notification for a single download.
// FollowedBy is filled on completion events for downloads that spawn a
// child download, such as magnet and followed torrent-file metadata. The
// child GIDs let callers track the download that holds the real files.
type Event struct {
	GID        string
	Type       EventType
	FollowedBy []string
}

// File describes a single file that belongs to a download.
type File struct {
	Index           int
	Path            string
	Length          int64
	CompletedLength int64
	Selected        bool
}

// DownloadInfo is the per-download status snapshot queried by the bot.
type DownloadInfo struct {
	GID             string
	Status          Status
	TotalLength     int64
	CompletedLength int64
	DownloadSpeed   int64
	UploadSpeed     int64
	Files           []File
	ErrorMessage    string
	FollowedBy      []string
	Following       string
	InfoHash        string
	Dir             string
}

// AddOptions carries per-download overrides. A nil *AddOptions uses the
// engine defaults from Config.
type AddOptions struct {
	// Dir is the output directory for this download. Each mirror request
	// passes its own unique directory.
	Dir string

	// Out is the output file name for single-file downloads.
	Out string
}

// Config is the engine-level configuration.
type Config struct {
	// DownloadDir is the base directory for downloads that do not carry a
	// per-download directory.
	DownloadDir string

	// MaxConcurrent caps the number of simultaneously active downloads.
	// Zero means one download at a time.
	MaxConcurrent int

	// CACertPath points to a PEM file added to the TLS root set for HTTPS
	// downloads. Empty uses the system roots.
	CACertPath string
}
