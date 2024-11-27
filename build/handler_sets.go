package build

import (
	"context"
	"log/slog"

	btclogv1 "github.com/btcsuite/btclog"
	"github.com/btcsuite/btclog/v2"
)

// handlerSet is an implementation of Handler that abstracts away multiple
// Handlers.
type handlerSet struct {
	level btclogv1.Level
	set   []btclog.Handler
}

// newHandlerSet constructs a new HandlerSet.
func newHandlerSet(level btclogv1.Level, set ...btclog.Handler) *handlerSet {
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
func (h *handlerSet) SetLevel(level btclogv1.Level) {
	for _, handler := range h.set {
		handler.SetLevel(level)
	}
	h.level = level
}

// Level returns the current logging level of the Handler.
//
// NOTE: this is part of the btclog.Handler interface.
func (h *handlerSet) Level() btclogv1.Level {
	return h.level
}

// WithPrefix returns a copy of the Handler but with the given string prefixed
// to each log message.
//
// NOTE: this is part of the btclog.Handler interface.
func (h *handlerSet) WithPrefix(prefix string) btclog.Handler {
	newSet := &handlerSet{set: make([]btclog.Handler, len(h.set))}
	for i, handler := range h.set {
		newSet.set[i] = handler.WithPrefix(prefix)
	}

	return newSet
}

// A compile-time check to ensure that handlerSet implements btclog.Handler.
var _ btclog.Handler = (*handlerSet)(nil)

// reducedSet is an implementation of the slog.Handler interface which is
// itself backed by multiple slog.Handlers. This is used by the handlerSet
// WithGroup and WithAttrs methods so that we can apply the WithGroup and
// WithAttrs to the underlying handlers in the set. These calls, however,
// produce slog.Handlers and not btclog.Handlers. So the reducedSet represents
// the resulting set produced.
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

// subLogGenerator implements the SubLogCreator backed by a Handler.
type subLogGenerator struct {
	handler btclog.Handler
}

// newSubLogGenerator constructs a new subLogGenerator from a Handler.
func newSubLogGenerator(handler btclog.Handler) *subLogGenerator {
	return &subLogGenerator{
		handler: handler,
	}
}

// Logger returns a new logger for a particular sub-system.
//
// NOTE: this is part of the SubLogCreator interface.
func (b *subLogGenerator) Logger(subsystemTag string) btclog.Logger {
	handler := b.handler.SubSystem(subsystemTag)

	return btclog.NewSLogger(handler)
}

// A compile-time check to ensure that handlerSet implements slog.Handler.
var _ SubLogCreator = (*subLogGenerator)(nil)
