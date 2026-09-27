package storage

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestDisk(t *testing.T) {
	ctx := context.Background()
	d, err := NewDisk(filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := d.Put(ctx, "../escape.txt", []byte("hi"))
	if err != nil || filepath.Dir(ref) != d.dir {
		t.Fatalf("put: %q %v (must stay inside the directory)", ref, err)
	}
	if b, err := d.Get(ctx, ref); err != nil || string(b) != "hi" {
		t.Fatalf("get: %q %v", b, err)
	}
	if err := d.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, ref); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := d.Delete(ctx, ref); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}
