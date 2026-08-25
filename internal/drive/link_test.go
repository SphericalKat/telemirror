package drive_test

import (
	"testing"

	"github.com/SphericalKat/telemirror/internal/drive"
)

func TestFileLinkUsesUpstreamFormat(t *testing.T) {
	got := drive.FileLink("1AbC_dEf")
	want := "https://drive.google.com/uc?id=1AbC_dEf&export=download"
	if got != want {
		t.Fatalf("FileLink() = %q, want %q", got, want)
	}
}

func TestFolderLinkUsesUpstreamFormat(t *testing.T) {
	got := drive.FolderLink("1AbC_dEf")
	want := "https://drive.google.com/drive/folders/1AbC_dEf"
	if got != want {
		t.Fatalf("FolderLink() = %q, want %q", got, want)
	}
}
