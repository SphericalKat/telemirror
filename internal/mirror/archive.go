package mirror

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// archiveResult returns the path to publish for a completed download. A
// single file is returned unchanged, because archiving it would change the
// result without need. A directory result is archived as a tar file created
// next to it, named after the directory, so the archive holds the directory
// as its single top-level entry. The returned size is the size of the path
// to publish.
func archiveResult(root string) (string, int64, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", 0, fmt.Errorf("archive %s: %w", root, err)
	}
	if !info.IsDir() {
		return root, info.Size(), nil
	}

	tarPath := root + ".tar"
	if err := writeTar(root, tarPath); err != nil {
		_ = os.Remove(tarPath)
		return "", 0, err
	}
	st, err := os.Stat(tarPath)
	if err != nil {
		_ = os.Remove(tarPath)
		return "", 0, fmt.Errorf("archive %s: %w", tarPath, err)
	}
	return tarPath, st.Size(), nil
}

// writeTar archives root into tarPath. The archive contains root itself and
// all of its content, with entry names relative to root's parent directory.
func writeTar(root, tarPath string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return fmt.Errorf("create archive %s: %w", tarPath, err)
	}

	parent := filepath.Dir(root)
	tw := tar.NewWriter(f)
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(parent, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if entry.IsDir() && !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write archive entry %s: %w", header.Name, err)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer src.Close()
		if _, err := io.Copy(tw, src); err != nil {
			return fmt.Errorf("copy %s: %w", path, err)
		}
		return nil
	})
	if walkErr != nil {
		_ = f.Close()
		return fmt.Errorf("archive %s: %w", root, walkErr)
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("close archive %s: %w", tarPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close archive %s: %w", tarPath, err)
	}
	return nil
}
