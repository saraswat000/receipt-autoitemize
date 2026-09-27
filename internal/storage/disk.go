// Package storage keeps uploaded receipt files. Disk is the local implementation;
// object storage (S3) would be another type with the same three methods.
package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Disk stores files in one local directory. A file's reference is its path.
type Disk struct{ dir string }

// NewDisk creates dir if needed.
func NewDisk(dir string) (*Disk, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Disk{dir: dir}, nil
}

// Put writes data under name (a base name; any directories are stripped).
func (d *Disk) Put(_ context.Context, name string, data []byte) (string, error) {
	path := filepath.Join(d.dir, filepath.Base(name))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// Get reads a stored file. A missing file wraps fs.ErrNotExist.
func (d *Disk) Get(_ context.Context, ref string) ([]byte, error) { return os.ReadFile(ref) }

// Delete removes a stored file; deleting a missing file is not an error.
func (d *Disk) Delete(_ context.Context, ref string) error {
	if err := os.Remove(ref); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
