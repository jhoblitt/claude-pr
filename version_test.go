package main

import "testing"

func TestVersionStringOverride(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "v1.2.3"
	if got := versionString(); got != "v1.2.3" {
		t.Errorf("versionString() = %q, want v1.2.3", got)
	}
}

func TestVersionStringFallback(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = ""
	// Without an injected tag it must still report something non-empty from the
	// build info (module version or VCS revision), never "".
	if got := versionString(); got == "" {
		t.Error("versionString() = empty, want a devel/module/vcs version")
	}
}
