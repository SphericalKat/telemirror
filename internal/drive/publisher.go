package drive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
)

// folderMIMEType is the Drive MIME type for folders.
const folderMIMEType = "application/vnd.google-apps.folder"

// fallbackMIMEType is used when the file extension has no known MIME type.
const fallbackMIMEType = "application/octet-stream"

// Service is the Drive boundary used by Publisher. The real implementation
// talks to Google Drive; tests replace it with a fake.
type Service interface {
	// CreateFolder creates a folder under parentID and returns its Drive ID.
	CreateFolder(ctx context.Context, name, parentID string) (string, error)

	// UploadFile uploads one file under parentID and returns its Drive ID.
	// onProgress reports bytes of this file sent so far and may be nil.
	UploadFile(ctx context.Context, path, name, mimeType, parentID string, size int64, onProgress func(sent int64)) (string, error)

	// GrantPublicRead lets anyone with the link read fileID.
	GrantPublicRead(ctx context.Context, fileID string) error

	// GrantReadAccess lets one email address read fileID.
	GrantReadAccess(ctx context.Context, fileID, email string) error
}

// PrivateSharing configures reader access for specific email addresses.
type PrivateSharing struct {
	Enabled bool
	Emails  []string
}

// Config configures one publisher.
type Config struct {
	// ParentFolderID is the Drive folder that receives the published result.
	ParentFolderID string

	// Private selects reader sharing for configured emails instead of
	// public sharing when Enabled is true.
	Private PrivateSharing

	// SharedDrive reports whether the parent folder lives in a Shared
	// Drive. Folders in a Shared Drive keep the upstream limitation: they
	// are not shared, but their link is still returned.
	SharedDrive bool
}

// Progress reports cumulative upload progress for one Publish call.
type Progress struct {
	UploadedBytes int64
	TotalBytes    int64
}

// Result describes the published result.
type Result struct {
	// DriveID is the Drive ID of the uploaded file or root folder.
	DriveID string

	// Name is the file or root folder name.
	Name string

	// IsFolder reports whether the published result is a folder.
	IsFolder bool

	// Link is the upstream-style link to the published result.
	Link string
}

// Publisher publishes a downloaded file or directory to Drive.
type Publisher struct {
	svc Service
	cfg Config
}

// NewPublisher returns a publisher for the configured Drive destination.
func NewPublisher(svc Service, cfg Config) (*Publisher, error) {
	if svc == nil {
		return nil, errors.New("drive publisher requires a Drive service")
	}
	if cfg.ParentFolderID == "" {
		return nil, errors.New("drive publisher requires a parent folder ID")
	}
	return &Publisher{svc: svc, cfg: cfg}, nil
}

// Publish uploads root, a file or a directory, to the configured parent
// folder. Directories upload recursively with their structure preserved, one
// entry at a time in lexical order. Progress reports are cumulative over all
// files. The published result receives the configured sharing.
func (p *Publisher) Publish(ctx context.Context, root string, onProgress func(Progress)) (Result, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Result{}, fmt.Errorf("publish %s: %w", root, err)
	}
	total, err := totalFileBytes(root)
	if err != nil {
		return Result{}, fmt.Errorf("publish %s: %w", root, err)
	}
	name := filepath.Base(root)
	tracker := newProgressTracker(total, onProgress)

	if info.IsDir() {
		folderID, err := p.svc.CreateFolder(ctx, name, p.cfg.ParentFolderID)
		if err != nil {
			return Result{}, fmt.Errorf("create folder %s: %w", name, err)
		}
		if err := p.uploadDir(ctx, root, folderID, tracker); err != nil {
			return Result{}, err
		}
		if err := p.share(ctx, folderID, true); err != nil {
			return Result{}, err
		}
		return Result{DriveID: folderID, Name: name, IsFolder: true, Link: FolderLink(folderID)}, nil
	}

	fileID, err := p.uploadFile(ctx, root, name, p.cfg.ParentFolderID, info.Size(), tracker)
	if err != nil {
		return Result{}, err
	}
	if err := p.share(ctx, fileID, false); err != nil {
		return Result{}, err
	}
	return Result{DriveID: fileID, Name: name, IsFolder: false, Link: FileLink(fileID)}, nil
}

// uploadDir uploads every entry of dir under parentID, one entry at a time
// in lexical order, depth first. Empty folders are still created.
func (p *Publisher) uploadDir(ctx context.Context, dir, parentID string, tracker *progressTracker) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("publish %s: %w", entry.Name(), err)
		}
		if entry.IsDir() {
			folderID, err := p.svc.CreateFolder(ctx, entry.Name(), parentID)
			if err != nil {
				return fmt.Errorf("create folder %s: %w", entry.Name(), err)
			}
			if err := p.uploadDir(ctx, filepath.Join(dir, entry.Name()), folderID, tracker); err != nil {
				return err
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		path := filepath.Join(dir, entry.Name())
		if _, err := p.uploadFile(ctx, path, entry.Name(), parentID, info.Size(), tracker); err != nil {
			return err
		}
	}
	return nil
}

// uploadFile uploads one file and advances the cumulative progress tracker.
func (p *Publisher) uploadFile(ctx context.Context, path, name, parentID string, size int64, tracker *progressTracker) (string, error) {
	id, err := p.svc.UploadFile(ctx, path, name, mimeTypeFor(path), parentID, size, tracker.fileReporter())
	if err != nil {
		return "", fmt.Errorf("upload %s: %w", name, err)
	}
	tracker.fileDone(size)
	return id, nil
}

// share applies the configured sharing to the published result. Shared
// Drive folders keep the upstream limitation: they are never shared.
func (p *Publisher) share(ctx context.Context, fileID string, isFolder bool) error {
	if isFolder && p.cfg.SharedDrive {
		return nil
	}
	if p.cfg.Private.Enabled {
		for _, email := range p.cfg.Private.Emails {
			if err := p.svc.GrantReadAccess(ctx, fileID, email); err != nil {
				return fmt.Errorf("grant reader access on %s to %s: %w", fileID, email, err)
			}
		}
		return nil
	}
	if err := p.svc.GrantPublicRead(ctx, fileID); err != nil {
		return fmt.Errorf("grant public access on %s: %w", fileID, err)
	}
	return nil
}

// mimeTypeFor returns the MIME type for a path, falling back to the generic
// binary type as the upstream bot does.
func mimeTypeFor(path string) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	return fallbackMIMEType
}

// totalFileBytes sums the sizes of all files under root, in any order.
func totalFileBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// progressTracker turns per-file byte reports into cumulative Publish progress.
type progressTracker struct {
	total    int64
	uploaded int64
	report   func(Progress)
}

func newProgressTracker(total int64, report func(Progress)) *progressTracker {
	return &progressTracker{total: total, report: report}
}

// fileReporter returns the reporter for the next file.
func (t *progressTracker) fileReporter() func(sent int64) {
	if t.report == nil {
		return nil
	}
	base := t.uploaded
	return func(sent int64) {
		t.report(Progress{UploadedBytes: base + sent, TotalBytes: t.total})
	}
}

// fileDone records a completed file so the next file reports cumulative bytes.
func (t *progressTracker) fileDone(size int64) {
	t.uploaded += size
}
