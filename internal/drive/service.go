package drive

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// APIService publishes through the official Google Drive client using an
// OAuth-authorized HTTP client.
type APIService struct {
	svc *drive.Service
}

var _ Service = (*APIService)(nil)

// NewAPIService returns the Drive adapter for an authorized HTTP client.
func NewAPIService(ctx context.Context, client *http.Client) (*APIService, error) {
	svc, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("create Drive service: %w", err)
	}
	return &APIService{svc: svc}, nil
}

// CreateFolder creates a folder under parentID and returns its Drive ID.
func (s *APIService) CreateFolder(ctx context.Context, name, parentID string) (string, error) {
	file, err := s.svc.Files.Create(&drive.File{
		Name:     name,
		MimeType: folderMIMEType,
		Parents:  []string{parentID},
	}).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id").
		Do()
	if err != nil {
		return "", fmt.Errorf("create Drive folder %s: %w", name, err)
	}
	return file.Id, nil
}

// UploadFile uploads one file under parentID and returns its Drive ID.
// Empty files are created without media, as the upstream bot does.
// onProgress reports bytes of this file sent so far and may be nil.
func (s *APIService) UploadFile(ctx context.Context, path, name, mimeType, parentID string, size int64, onProgress func(sent int64)) (string, error) {
	call := s.svc.Files.Create(&drive.File{
		Name:     name,
		MimeType: mimeType,
		Parents:  []string{parentID},
	}).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id")

	if size == 0 {
		file, err := call.Do()
		if err != nil {
			return "", fmt.Errorf("upload %s to Drive: %w", name, err)
		}
		return file.Id, nil
	}

	reader, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer reader.Close()

	var sent int64
	if onProgress != nil {
		call.Media(reader, googleapi.ContentType(mimeType)).
			ProgressUpdater(func(current, _ int64) {
				sent = current
				onProgress(current)
			})
	} else {
		call.Media(reader, googleapi.ContentType(mimeType))
	}

	file, err := call.Do()
	if err != nil {
		return "", fmt.Errorf("upload %s to Drive: %w", name, err)
	}
	// Files below the resumable chunk size upload in one request and never
	// call the progress updater, so report the completed size once.
	if onProgress != nil && sent < size {
		onProgress(size)
	}
	return file.Id, nil
}

// GrantPublicRead lets anyone with the link read fileID.
func (s *APIService) GrantPublicRead(ctx context.Context, fileID string) error {
	_, err := s.svc.Permissions.Create(fileID, &drive.Permission{
		Role: "reader",
		Type: "anyone",
	}).
		Context(ctx).
		SupportsAllDrives(true).
		Do()
	if err != nil {
		return fmt.Errorf("grant public reader access on %s: %w", fileID, err)
	}
	return nil
}

// GrantReadAccess lets one email address read fileID.
func (s *APIService) GrantReadAccess(ctx context.Context, fileID, email string) error {
	_, err := s.svc.Permissions.Create(fileID, &drive.Permission{
		Role:         "reader",
		Type:         "user",
		EmailAddress: email,
	}).
		Context(ctx).
		SupportsAllDrives(true).
		Do()
	if err != nil {
		return fmt.Errorf("grant reader access on %s to %s: %w", fileID, email, err)
	}
	return nil
}
