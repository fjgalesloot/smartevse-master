package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
)

// nodeFirst is a text log handler that writes the node attribute before the
// message, so the lines of one node are easy to follow: debugging is mostly
// per node. Lines without a node look like slog's text format.
type nodeFirst struct {
	inner slog.Handler
	attrs []slog.Attr // from WithAttrs, written after the message
}

func newLogHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return &nodeFirst{inner: slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		// Handle passes the message as an attribute: drop the empty built-in one
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.MessageKey && a.Value.String() == "" {
				return slog.Attr{}
			}
			return a
		},
	})}
}

func (h *nodeFirst) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *nodeFirst) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, "", r.PC)
	all := slices.Clip(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		all = append(all, a)
		return true
	})
	var rest []slog.Attr
	for _, a := range all {
		if a.Key == "node" {
			out.AddAttrs(a)
		} else {
			rest = append(rest, a)
		}
	}
	out.AddAttrs(slog.String(slog.MessageKey, r.Message))
	out.AddAttrs(rest...)
	return h.inner.Handle(ctx, out)
}

func (h *nodeFirst) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &nodeFirst{inner: h.inner, attrs: append(slices.Clip(h.attrs), attrs...)}
}

// WithGroup is not used here; attributes added before it stay ahead of the message.
func (h *nodeFirst) WithGroup(name string) slog.Handler {
	return &nodeFirst{inner: h.inner.WithAttrs(h.attrs).WithGroup(name)}
}
