package main

import (
	"fmt"
	"runtime/debug"
)

// version is injected into release binaries via `-ldflags "-X main.version=<tag>"`
// (see the release workflow). For `go install`/`go build` it stays empty and
// versionString falls back to the module version or the VCS revision.
var version = ""

// versionString reports the build version: the injected release tag if present,
// else the module version from the build info, else a devel string built from
// the VCS revision (suffixed -dirty when the tree had uncommitted changes).
func versionString() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, mod string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			mod = s.Value
		}
	}
	if rev == "" {
		return "devel"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if mod == "true" {
		rev += "-dirty"
	}
	return "devel+" + rev
}

func printVersion() {
	fmt.Println("claude-pr " + versionString())
}
