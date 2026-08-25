package mirror

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/telegram"
)

// handleList searches the configured Drive folder for direct children whose
// names contain the search text and replies with the upstream result list.
func (s *Service) handleList(ctx context.Context, msg *telegram.Message, fileName string) {
	if fileName == "" {
		// The upstream bot requires a search term after /list.
		return
	}
	children, err := s.lister.List(ctx, fileName)
	if err != nil {
		log.Printf("mirror: list %q: %v", fileName, err)
		s.sendTemporaryReply(ctx, msg, "Failed to fetch the list of files")
		return
	}
	s.sendListReply(ctx, msg, listReply(children))
}

// handleGetFolder replies with the link to the configured Drive folder.
func (s *Service) handleGetFolder(ctx context.Context, msg *telegram.Message, arg string) {
	if arg != "" {
		// The upstream bot ignores /getFolder with arguments.
		return
	}
	text := fmt.Sprintf("<a href = '%s'>Drive mirror folder</a>", s.lister.FolderLink())
	s.sendListReply(ctx, msg, text)
}

// listReply renders search results the way the upstream bot does: one line
// per child with its link and the file size or a folder marker.
func listReply(children []drive.Child) string {
	if len(children) == 0 {
		return "There are no files matching your parameters"
	}
	var b strings.Builder
	for _, child := range children {
		fmt.Fprintf(&b, "<a href = '%s'>%s</a>", child.Link, child.Name)
		switch {
		case child.Size != nil:
			fmt.Fprintf(&b, " (%s)\n", formatSize(*child.Size))
		case child.IsFolder:
			b.WriteString(" (folder)\n")
		default:
			b.WriteString("\n")
		}
	}
	return b.String()
}
