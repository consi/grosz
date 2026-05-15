// Package events provides typed instrumentation helpers for recording
// system events to the store. Use events.For(SourceXxx, store) once in a
// component's constructor, then call .Info/.Warn/.Error with an Action
// constant from this package — never write a literal action string.
package events

import "time"

// Recorder is the minimal persistence surface the events package needs.
// *store.Store satisfies this; tests use NewMemoryRecorder.
type Recorder interface {
	RecordSystemEvent(SystemEvent) error
}

// Source identifies the component emitting the event.
type Source string

// Action identifies what happened. All values are camelCase strings.
type Action string

// Level describes severity.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Bound is a recorder pre-bound to a Source. Construct via For.
type Bound struct {
	rec    Recorder
	source Source
}

// For returns a recorder bound to the given source. nil-safe — calls on
// a Bound with nil recorder are no-ops, so tests that don't supply a
// store still work.
func For(source Source, rec Recorder) *Bound {
	return &Bound{rec: rec, source: source}
}

func (b *Bound) emit(level Level, action Action, input, result any) {
	if b == nil || b.rec == nil {
		return
	}
	_ = b.rec.RecordSystemEvent(SystemEvent{
		Timestamp: time.Now(),
		Source:    string(b.source),
		Action:    string(action),
		Level:     string(level),
		Input:     input,
		Result:    result,
	})
}

// Info records a successful action.
func (b *Bound) Info(a Action, input, result any) {
	b.emit(LevelInfo, a, input, result)
}

// Warn records a non-fatal anomaly.
func (b *Bound) Warn(a Action, input, result any) {
	b.emit(LevelWarn, a, input, result)
}

// Error records a failure. The error message is wrapped into the result
// payload as {"error": err.Error()}.
func (b *Bound) Error(a Action, input any, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	b.emit(LevelError, a, input, map[string]any{"error": msg})
}

// Info is a convenience for one-off cross-source emissions where binding
// a recorder is overkill (e.g. cmd/grosz/main.go, internal/store/*).
func Info(rec Recorder, src Source, a Action, input, result any) {
	For(src, rec).Info(a, input, result)
}

// Warn is the convenience equivalent of Bound.Warn.
func Warn(rec Recorder, src Source, a Action, input, result any) {
	For(src, rec).Warn(a, input, result)
}

// Error is the convenience equivalent of Bound.Error.
func Error(rec Recorder, src Source, a Action, input any, err error) {
	For(src, rec).Error(a, input, err)
}
