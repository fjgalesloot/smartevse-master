package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"
)

// logEntry is one log record as the web UI shows it.
type logEntry struct {
	Seq   uint64      `json:"seq"`
	Time  time.Time   `json:"time"`
	Level string      `json:"level"`
	Node  string      `json:"node,omitempty"`
	Msg   string      `json:"msg"`
	Attrs [][2]string `json:"attrs,omitempty"` // key, value in record order
}

// logRing keeps the last records in memory for the web UI.
type logRing struct {
	mu   sync.Mutex
	buf  []logEntry
	next int // index of the oldest entry once the buffer is full
	seq  uint64
}

func newLogRing(size int) *logRing {
	if size < 1 {
		size = 1
	}
	return &logRing{buf: make([]logEntry, 0, size)}
}

func (r *logRing) add(e logEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	if len(r.buf) < cap(r.buf) {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
}

// last returns the newest sequence number and the buffer size.
func (r *logRing) last() (uint64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq, cap(r.buf)
}

// since returns up to limit entries with a sequence number above after,
// oldest first; with more available it returns the newest ones.
func (r *logRing) since(after uint64, limit int) []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]logEntry, 0)
	for i := range r.buf {
		e := r.buf[(r.next+i)%len(r.buf)]
		if e.Seq > after {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// ringHandler is a slog.Handler writing into a logRing.
type ringHandler struct {
	ring   *logRing
	level  slog.Leveler
	attrs  []slog.Attr
	prefix string // from WithGroup
}

func (h *ringHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *ringHandler) Handle(_ context.Context, r slog.Record) error {
	e := logEntry{Time: r.Time, Level: r.Level.String(), Msg: r.Message}
	add := func(a slog.Attr) bool {
		h.addAttr(&e, "", a)
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	h.ring.add(e)
	return nil
}

func (h *ringHandler) addAttr(e *logEntry, group string, a slog.Attr) {
	v := a.Value.Resolve()
	if a.Key == "" && v.Kind() != slog.KindGroup {
		return
	}
	key := a.Key
	if group != "" && key != "" {
		key = group + "." + key
	}
	if v.Kind() == slog.KindGroup {
		for _, g := range v.Group() {
			h.addAttr(e, key, g)
		}
		return
	}
	key = h.prefix + key
	if key == "node" && e.Node == "" {
		e.Node = v.String()
		return
	}
	e.Attrs = append(e.Attrs, [2]string{key, formatValue(v)})
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindTime:
		return v.Time().Format("15:04:05.000")
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'f', -1, 64)
	}
	return v.String()
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	if c.prefix != "" {
		attrs = slices.Clone(attrs)
		for i := range attrs {
			attrs[i].Key = c.prefix + attrs[i].Key
		}
	}
	c.attrs = append(slices.Clip(h.attrs), attrs...)
	return &c
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.prefix = h.prefix + name + "."
	return &c
}

// teeHandler sends each record to every handler that wants its level.
type teeHandler []slog.Handler

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range t {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithGroup(name)
	}
	return out
}
