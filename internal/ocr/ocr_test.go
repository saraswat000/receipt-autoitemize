package ocr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStubEngine(t *testing.T) {
	dir := t.TempDir()
	fixtures := filepath.Join(dir, "fixtures")
	os.MkdirAll(fixtures, 0o755)
	os.WriteFile(filepath.Join(fixtures, "receipt-clean.txt"), []byte("MERCHANT: Cafe"), 0o644)
	upload := []byte("TOTAL   1.00")

	e := StubEngine{FixturesDir: fixtures}
	ctx := context.Background()

	if got, err := e.ExtractText(ctx, Input{Data: upload, Filename: "x.txt", ContentType: "text/plain; charset=utf-8"}); err != nil || got != "TOTAL   1.00" {
		t.Fatalf("text upload: %q %v", got, err)
	}
	if got, err := e.ExtractText(ctx, Input{Data: upload, Filename: "receipt-clean.jpg", ContentType: "image/jpeg"}); err != nil || !strings.Contains(got, "Cafe") {
		t.Fatalf("image by name: %q %v", got, err)
	}
	// Path traversal in the client file name cannot escape the fixtures dir.
	if _, err := e.ExtractText(ctx, Input{Data: upload, Filename: "../../etc/passwd", ContentType: "image/png"}); !errors.Is(err, ErrNoText) {
		t.Fatalf("traversal: %v", err)
	}
	if _, err := e.ExtractText(ctx, Input{Data: upload, Filename: "unknown.png", ContentType: "image/png"}); !errors.Is(err, ErrNoText) {
		t.Fatalf("unknown: %v", err)
	}
}
