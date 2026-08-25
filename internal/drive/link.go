package drive

import "fmt"

// FileLink returns the upstream-style link for a file.
func FileLink(fileID string) string {
	return fmt.Sprintf("https://drive.google.com/uc?id=%s&export=download", fileID)
}

// FolderLink returns the upstream-style link for a folder.
func FolderLink(folderID string) string {
	return fmt.Sprintf("https://drive.google.com/drive/folders/%s", folderID)
}
