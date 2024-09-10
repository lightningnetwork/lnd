package build

import (
	"context"
	"log/slog"

	"github.com/btcsuite/btclog"
)

// handlerSet is an implementation of Handler that abstracts away multiple
// Handlers.
type handlerSet struct {
	level btclog.Level
	set   []btclog.Handler
}

// newHandlerSet constructs a new HandlerSet.
func newHandlerSet(level btclog.Level, set ...btclog.Handler) *handlerSet {
	h := &handlerSet{
		set:   set,
		level: level,
	}
	h.SetLevel(level)

	return h
}

// Enabled reports whether the handler handles records at the given level.
//
// NOTE: this is part of the slog.Handler interface.
func (h *handlerSet) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.set {
		if !handler.Enabled(ctx, level) {
			return false
		}
	}

	return true
}

// Handle handles the Record.
//
// NOTE: this is part of the slog.Handler interface.
func (h *handlerSet) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range h.set {
		if err := handler.Handle(ctx, record); err != nil {
			return err
		}
	}

	return nil
}

// WithAttrs returns a new Handler whose attributes consist of both the
// receiver's attributes and the arguments.
//
// NOTE: this is part of the slog.Handler interface.
func (h *handlerSet) WithAttrs(attrs []slog.Attr) slog.Handler {
	newSet := &reducedSet{set: make([]slog.Handler, len(h.set))}
	for i, handler := range h.set {
		newSet.set[i] = handler.WithAttrs(attrs)
	}

	return newSet
}

// WithGroup returns a new Handler with the given group appended to the
// receiver's existing groups.
//
// NOTE: this is part of the slog.Handler interface.
func (h *handlerSet) WithGroup(name string) slog.Handler {
	newSet := &reducedSet{set: make([]slog.Handler, len(h.set))}
	for i, handler := range h.set {
		newSet.set[i] = handler.WithGroup(name)
	}

	return newSet
}

// SubSystem creates a new Handler with the given sub-system tag.
//
// NOTE: this is part of the Handler interface.
func (h *handlerSet) SubSystem(tag string) btclog.Handler {
	newSet := &handlerSet{set: make([]btclog.Handler, len(h.set))}
	for i, handler := range h.set {
		newSet.set[i] = handler.SubSystem(tag)
	}

	return newSet
}

// SetLevel changes the logging level of the Handler to the passed
// level.
//
// NOTE: this is part of the btclog.Handler interface.
func (h *handlerSet) SetLevel(level btclog.Level) {
	for _, handler := range h.set {
		handler.SetLevel(level)
	}
	h.level = level
}

// Level returns the current logging level of the Handler.
//
// NOTE: this is part of the btclog.Handler interface.
func (h *handlerSet) Level() btclog.Level {
	return h.level
}

// A compile-time check to ensure that handlerSet implements btclog.Handler.
var _ btclog.Handler = (*handlerSet)(nil)

// reducedSet is an implementation of the slog.Handler interface which is
// itself backed by multiple slog.Handlers.
type reducedSet struct {
	set []slog.Handler
}

// Enabled reports whether the handler handles records at the given level.
//
// NOTE: this is part of the slog.Handler interface.
func (r *reducedSet) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range r.set {
		if !handler.Enabled(ctx, level) {
			return false
		}
	}

	return true
}

// Handle handles the Record.
//
// NOTE: this is part of the slog.Handler interface.
func (r *reducedSet) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range r.set {
		if err := handler.Handle(ctx, record); err != nil {
			return err
		}
	}

	return nil
}

// WithAttrs returns a new Handler whose attributes consist of both the
// receiver's attributes and the arguments.
//
// NOTE: this is part of the slog.Handler interface.
func (r *reducedSet) WithAttrs(attrs []slog.Attr) slog.Handler {
	newSet := &reducedSet{set: make([]slog.Handler, len(r.set))}
	for i, handler := range r.set {
		newSet.set[i] = handler.WithAttrs(attrs)
	}

	return newSet
}

// WithGroup returns a new Handler with the given group appended to the
// receiver's existing groups.
//
// NOTE: this is part of the slog.Handler interface.
func (r *reducedSet) WithGroup(name string) slog.Handler {
	newSet := &reducedSet{set: make([]slog.Handler, len(r.set))}
	for i, handler := range r.set {
		newSet.set[i] = handler.WithGroup(name)
	}

	return newSet
}

// A compile-time check to ensure that handlerSet implements slog.Handler.
var _ slog.Handler = (*reducedSet)(nil)
