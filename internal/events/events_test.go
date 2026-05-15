package events

import (
	"errors"
	"sync"
	"testing"
)

// MemoryRecorder captures recorded events in-memory for assertion in tests.
type MemoryRecorder struct {
	mu     sync.Mutex
	Events []SystemEvent
}

func NewMemoryRecorder() *MemoryRecorder { return &MemoryRecorder{} }

func (m *MemoryRecorder) RecordSystemEvent(e SystemEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Events = append(m.Events, e)
	return nil
}

func (m *MemoryRecorder) Last() SystemEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Events) == 0 {
		return SystemEvent{}
	}
	return m.Events[len(m.Events)-1]
}

func TestBoundInfoRecordsEvent(t *testing.T) {
	rec := NewMemoryRecorder()
	b := For(SourceScheduler, rec)
	b.Info(ActionRecompute, map[string]any{"k": 1}, map[string]any{"ok": true})

	if len(rec.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(rec.Events))
	}
	e := rec.Last()
	if e.Source != "scheduler" {
		t.Errorf("source = %q, want scheduler", e.Source)
	}
	if e.Action != "recompute" {
		t.Errorf("action = %q, want recompute", e.Action)
	}
	if e.Level != "info" {
		t.Errorf("level = %q, want info", e.Level)
	}
	if e.Timestamp.IsZero() {
		t.Errorf("timestamp not set")
	}
}

func TestBoundErrorWrapsErr(t *testing.T) {
	rec := NewMemoryRecorder()
	For(SourceMeter, rec).Error(ActionMeterPoll, nil, errors.New("boom"))

	e := rec.Last()
	if e.Level != "error" {
		t.Errorf("level = %q, want error", e.Level)
	}
	res, ok := e.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", e.Result)
	}
	if res["error"] != "boom" {
		t.Errorf("result error = %v, want boom", res["error"])
	}
}

func TestBoundErrorWithNilErr(t *testing.T) {
	rec := NewMemoryRecorder()
	For(SourceMeter, rec).Error(ActionMeterPoll, nil, nil)

	e := rec.Last()
	res, _ := e.Result.(map[string]any)
	if res["error"] != "" {
		t.Errorf("result error = %v, want empty", res["error"])
	}
}

func TestNilRecorderIsNoop(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil recorder should not panic, got %v", r)
		}
	}()
	For(SourceApp, nil).Info(ActionAppStarted, nil, nil)
}

func TestConvenienceHelpers(t *testing.T) {
	rec := NewMemoryRecorder()
	Info(rec, SourceCosts, ActionCostsAdd, "in", "res")
	Warn(rec, SourceCosts, ActionCostsDelete, nil, nil)
	Error(rec, SourceStore, ActionIdleSnapshotDaily, nil, errors.New("x"))

	if len(rec.Events) != 3 {
		t.Fatalf("want 3 events, got %d", len(rec.Events))
	}
	if rec.Events[0].Level != "info" || rec.Events[1].Level != "warn" || rec.Events[2].Level != "error" {
		t.Errorf("levels = %v %v %v", rec.Events[0].Level, rec.Events[1].Level, rec.Events[2].Level)
	}
}

func TestRedactSettingsHidesSecrets(t *testing.T) {
	in := map[string]string{
		"tariff.pstryk_token": "sk-abcdef",
		"auth.password":       "hunter2",
		"api_key":             "k123",
		"some.secret":         "shh",
		"renault.username":    "alice",
		"empty.token":         "",
	}
	out := RedactSettings(in)

	for _, k := range []string{"tariff.pstryk_token", "auth.password", "api_key", "some.secret"} {
		if out[k] != "****" {
			t.Errorf("%s = %q, want ****", k, out[k])
		}
	}
	if out["renault.username"] != "alice" {
		t.Errorf("non-sensitive value clobbered: %q", out["renault.username"])
	}
	if out["empty.token"] != "" {
		t.Errorf("empty value should pass through, got %q", out["empty.token"])
	}
}
