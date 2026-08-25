package drive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxListResults caps a search at the newest 20 children, like the upstream
// bot.
const maxListResults = 20

// Child describes one direct child of a Drive folder.
type Child struct {
	// ID is the Drive ID of the file or folder.
	ID string

	// Name is the name shown to the user.
	Name string

	// IsFolder reports whether the child is a folder.
	IsFolder bool

	// Size is the size of a file child in bytes. It is nil when Drive
	// reports no size, as for folders.
	Size *int64

	// ModifiedTime is the time of the last modification. Newer children
	// come first in a listing.
	ModifiedTime time.Time

	// Link is the upstream-style link to the child.
	Link string
}

// Lister searches the configured Drive destination.
type Lister struct {
	svc      Service
	parentID string
}

// NewLister returns a lister for the configured Drive destination.
func NewLister(svc Service, cfg Config) (*Lister, error) {
	if svc == nil {
		return nil, errors.New("drive lister requires a Drive service")
	}
	if cfg.ParentFolderID == "" {
		return nil, errors.New("drive lister requires a parent folder ID")
	}
	return &Lister{svc: svc, parentID: cfg.ParentFolderID}, nil
}

// List returns the direct children of the configured folder whose names
// contain fileName, or one of the upstream separator variants when fileName
// contains spaces. Results are ordered by newest modification first and hold
// at most 20 children.
func (l *Lister) List(ctx context.Context, fileName string) ([]Child, error) {
	children, err := l.svc.ListChildren(ctx, l.parentID, searchVariants(fileName))
	if err != nil {
		return nil, fmt.Errorf("search Drive children matching %q: %w", fileName, err)
	}

	sort.SliceStable(children, func(i, j int) bool {
		return children[i].ModifiedTime.After(children[j].ModifiedTime)
	})
	if len(children) > maxListResults {
		children = children[:maxListResults]
	}
	for i := range children {
		children[i].Link = linkFor(children[i])
	}
	return children, nil
}

// FolderLink returns the upstream-style link to the configured folder.
func (l *Lister) FolderLink() string {
	return FolderLink(l.parentID)
}

// searchVariants returns the name variations the upstream bot searches for:
// the original name and, when it contains spaces, the same name with spaces
// replaced by dots, then all dots by dashes, then all dashes by underscores.
func searchVariants(fileName string) []string {
	if !strings.Contains(fileName, " ") {
		return []string{fileName}
	}
	dots := strings.ReplaceAll(fileName, " ", ".")
	dashes := strings.ReplaceAll(dots, ".", "-")
	underscores := strings.ReplaceAll(dashes, "-", "_")
	return []string{fileName, dots, dashes, underscores}
}

// linkFor returns the upstream-style link for a child.
func linkFor(child Child) string {
	if child.IsFolder {
		return FolderLink(child.ID)
	}
	return FileLink(child.ID)
}
