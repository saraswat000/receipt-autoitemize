// Package ocr turns an uploaded receipt file into raw text.
//
// OCR IS STUBBED: no vendor is called and no API key is needed. StubEngine
// treats the fixtures as "already known images":
//
//  1. A plain-text upload (e.g. fixtures/task-a/receipt-clean.txt) is its own OCR text.
//  2. An image/PDF whose file name matches a fixture (receipt-clean.jpg, receipt-clean.pdf)
//     returns that fixture's text.
//  3. Anything else fails with ErrNoText, and the receipt is marked OCR_FAILED.
//
// A real engine (Textract, Google Vision, a VLM) implements the same Engine interface
// and is selected in cmd/server.
package ocr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ErrNoText means the engine could not read any text from the file.
var ErrNoText = errors.New("no OCR text")

// Input describes the stored file to read.
type Input struct {
	Path        string // local path of the stored upload
	Filename    string // original client file name
	ContentType string // sniffed content type
}

// Engine extracts raw text from a receipt file.
type Engine interface {
	Name() string
	ExtractText(ctx context.Context, in Input) (string, error)
}

// StubEngine reads fixture text instead of running OCR.
type StubEngine struct {
	FixturesDir string
}

func (StubEngine) Name() string { return "stub-fixtures" }

func (e StubEngine) ExtractText(_ context.Context, in Input) (string, error) {
	if strings.HasPrefix(in.ContentType, "text/plain") {
		b, err := os.ReadFile(in.Path)
		if err != nil {
			return "", fmt.Errorf("read upload: %w", err)
		}
		if !utf8.Valid(b) || strings.TrimSpace(string(b)) == "" {
			return "", fmt.Errorf("%w: text upload is empty or not UTF-8", ErrNoText)
		}
		return string(b), nil
	}

	stem := strings.TrimSuffix(filepath.Base(in.Filename), filepath.Ext(in.Filename))
	if stem != "" && e.FixturesDir != "" {
		b, err := os.ReadFile(filepath.Join(e.FixturesDir, stem+".txt"))
		if err == nil {
			return string(b), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read fixture: %w", err)
		}
	}
	return "", fmt.Errorf("%w: stub OCR has no text for %q; upload a fixture .txt or an image named after one (e.g. receipt-clean.jpg)", ErrNoText, in.Filename)
}
