package main

import (
	"runtime/debug"
	"sync"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3" (Dockerfile, CI).
var version string

// buildVersion is the version set at build time, else the git revision go build
// embeds (dev-<rev>[-dirty]), else "dev".
var buildVersion = sync.OnceValue(func() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var (
		rev   string
		dirty bool
	)
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	v := "dev-" + rev[:min(7, len(rev))]
	if dirty {
		v += "-dirty"
	}
	return v
})
