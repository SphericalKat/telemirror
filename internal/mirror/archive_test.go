package mirror

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveResultSingleFileIsUnchanged(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.bin")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	gotPath, size, err := archiveResult(path)
	if err != nil {
		t.Fatalf("archiveResult() error = %v", err)
	}
	if gotPath != path {
		t.Errorf("archiveResult() path = %q, want the file itself %q", gotPath, path)
	}
	if size != int64(len("payload")) {
		t.Errorf("archiveResult() size = %d, want %d", size, len("payload"))
	}
}

func TestArchiveResultDirectoryCreatesTarWithTopLevelFolder(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "pack")
	if err := os.MkdirAll(filepath.Join(pack, "sub"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pack, "a.bin"), []byte("alpha"), 0o600); err != nil {
		t.Fatalf("write a.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pack, "sub", "b.bin"), []byte("beta"), 0o600); err != nil {
		t.Fatalf("write b.bin: %v", err)
	}

	gotPath, size, err := archiveResult(pack)
	if err != nil {
		t.Fatalf("archiveResult() error = %v", err)
	}
	if want := pack + ".tar"; gotPath != want {
		t.Fatalf("archiveResult() path = %q, want %q", gotPath, want)
	}
	if size <= 0 {
		t.Errorf("archiveResult() size = %d, want a positive archive size", size)
	}

	contents := readTarEntries(t, gotPath)
	wantNames := []string{"pack/", "pack/a.bin", "pack/sub/", "pack/sub/b.bin"}
	if len(contents) != len(wantNames) {
		t.Fatalf("archive entries = %v, want %v", entryNames(contents), wantNames)
	}
	for i, want := range wantNames {
		if contents[i].name != want {
			t.Errorf("archive entry %d = %q, want %q", i, contents[i].name, want)
		}
	}
	if got := contents[1].data; string(got) != "alpha" {
		t.Errorf("pack/a.bin content = %q, want %q", got, "alpha")
	}
	if got := contents[3].data; string(got) != "beta" {
		t.Errorf("pack/sub/b.bin content = %q, want %q", got, "beta")
	}
}

func TestArchiveResultFailureRemovesPartialArchive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; a permission failure cannot be simulated")
	}
	root := t.TempDir()
	pack := filepath.Join(root, "pack")
	if err := os.MkdirAll(pack, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pack, "a.bin"), []byte("alpha"), 0o000); err != nil {
		t.Fatalf("write a.bin: %v", err)
	}

	_, _, err := archiveResult(pack)
	if err == nil {
		t.Fatal("archiveResult() error = nil, want a failure for an unreadable file")
	}
	if _, statErr := os.Stat(pack + ".tar"); !os.IsNotExist(statErr) {
		t.Errorf("partial archive still exists after failure: %v", statErr)
	}
}

type tarEntry struct {
	name string
	data []byte
}

func readTarEntries(t *testing.T, path string) []tarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var entries []tarEntry
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		entry := tarEntry{name: header.Name}
		if header.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s content: %v", header.Name, err)
			}
			entry.data = data
		}
		entries = append(entries, entry)
	}
}

func entryNames(entries []tarEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.name)
	}
	return names
}
