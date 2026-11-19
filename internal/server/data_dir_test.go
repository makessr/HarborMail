package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := checkDataDir(dir); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("write check left files: %v, %v", files, err)
	}
	if err := checkDataDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory: got %v, want an error matching fs.ErrNotExist", err)
	}
}
