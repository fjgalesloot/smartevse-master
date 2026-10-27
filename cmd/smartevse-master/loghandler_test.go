package main

import (
	"bytes"
	"log/slog"
	"regexp"
	"testing"
)

func TestLogHandlerNodeFirst(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newLogHandler(&buf, slog.LevelInfo))

	log.Info("node current", "from", 16, "node", 1, "to", 10.5)
	log.With("port", "tty").Info("bus connected", "timeout", "1s")
	log.Debug("cycle", "node", 2)

	want := []string{
		`level=INFO node=1 msg="node current" from=16 to=10.5`,
		`level=INFO msg="bus connected" port=tty timeout=1s`,
	}
	lines := regexp.MustCompile(`(?m)^time=\S+ (.*)$`).FindAllStringSubmatch(buf.String(), -1)
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i, l := range lines {
		if l[1] != want[i] {
			t.Errorf("line %d:\n got  %s\n want %s", i, l[1], want[i])
		}
	}
}
