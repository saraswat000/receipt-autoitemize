package reqid

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerStampsRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(Handler{slog.NewTextHandler(&buf, nil)}).With("component", "ocr")
	log.InfoContext(With(context.Background(), "req_42"), "ocr done")
	log.InfoContext(context.Background(), "no request")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[0], "request_id=req_42") || !strings.Contains(lines[0], "component=ocr") {
		t.Errorf("first line: %s", lines[0])
	}
	if strings.Contains(lines[1], "request_id") {
		t.Errorf("a record without a request must not get one: %s", lines[1])
	}
}
