package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SphericalKat/telemirror/internal/config"
)

func TestOpenStoreDisabledWithoutPath(t *testing.T) {
	if store := openStore(""); store != nil {
		t.Error("openStore with no path returned a store, want nil")
	}
	if store := openStore("   "); store != nil {
		t.Error("openStore with a blank path returned a store, want nil")
	}
}

func TestOpenStoreFallsBackOnUnusableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemirror.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database at all"), 0o600); err != nil {
		t.Fatalf("write corrupt database file: %v", err)
	}

	if store := openStore(path); store != nil {
		store.Close()
		t.Error("openStore on a non-database file returned a store, want the memory fallback")
	}
}

func TestOpenStoreOpensUsableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemirror.db")

	store := openStore(path)
	if store == nil {
		t.Fatal("openStore on a fresh path returned nil, want a store")
	}
	defer store.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file was not created: %v", err)
	}
}

func TestMirrorConfigWiresTheDiskReporter(t *testing.T) {
	out := mirrorConfig(config.Config{DownloadRoot: "/srv/mirror"}, nil)
	if out.DiskRoot != "/srv/mirror" {
		t.Errorf("DiskRoot = %q, want /srv/mirror", out.DiskRoot)
	}
	if out.DiskUsage == nil {
		t.Error("DiskUsage = nil, want the platform disk reporter")
	}
}
