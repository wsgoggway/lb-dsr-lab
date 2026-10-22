package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// captureHandler collects slog records for assertions.
type captureHandler struct {
	mu      sync.Mutex
	records []map[string]any
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := map[string]any{r.Message: true}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, rec)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if _, ok := r[msg]; ok {
			n++
		}
	}
	return n
}

func TestAuditRecordFields(t *testing.T) {
	h := &captureHandler{}
	logger := slog.New(h)

	Audit(logger, "operator1", "/pfkeeper.v1.TableService/SetTable",
		map[string]string{"table": "web", "reason": "scale-up"}, "ok commit=abc123")

	if h.count("audit") != 1 {
		t.Fatalf("audit records = %d, want 1", h.count("audit"))
	}
	rec := h.records[0]
	for key, want := range map[string]any{
		"event":   "audit",
		"actor":   "operator1",
		"method":  "/pfkeeper.v1.TableService/SetTable",
		"params":  "table=web,reason=scale-up",
		"outcome": "ok commit=abc123",
	} {
		if got := rec[key]; got != want {
			t.Errorf("field %s = %v, want %v", key, got, want)
		}
	}

	// Nil logger must not panic.
	Audit(nil, "a", "m", nil, "ok")
}

func TestAuditErrOutcome(t *testing.T) {
	h := &captureHandler{}
	logger := slog.New(h)

	AuditErr(logger, "viewer1", "/pfkeeper.v1.TableService/SetTable", map[string]string{"table": "web"},
		"denied", errPermission("viewer may only call read methods"))

	if h.count("audit") != 1 {
		t.Fatalf("audit records = %d, want 1", h.count("audit"))
	}
	outcome, _ := h.records[0]["outcome"].(string)
	if !strings.Contains(outcome, "denied") || !strings.Contains(outcome, "read methods") {
		t.Fatalf("outcome = %q, want denied + reason", outcome)
	}
}

type errPermission string

func (e errPermission) Error() string { return string(e) }
