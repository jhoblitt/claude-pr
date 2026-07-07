package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopEntry(t *testing.T) {
	e := desktopEntry("/home/me/.local/share/claude-pr/claude-resume-handler")
	for _, want := range []string{
		`Exec="/home/me/.local/share/claude-pr/claude-resume-handler" %u`,
		"MimeType=x-scheme-handler/claude-resume;",
		"NoDisplay=true",
	} {
		if !strings.Contains(e, want) {
			t.Errorf("desktop entry missing %q in:\n%s", want, e)
		}
	}
}

func TestResumeWrapperScriptValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "handler")
	if err := os.WriteFile(path, []byte(resumeWrapperScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n rejected the wrapper: %v\n%s", err, out)
	}
}

// TestResumeWrapperEndToEnd drives the generated wrapper with a fake terminal
// (injected via the override) and asserts what command it would launch — the
// real proof that the URL is parsed, decoded, and (for a bad id) refused.
func TestResumeWrapperEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	capPath := filepath.Join(dir, "captured")
	fake := filepath.Join(dir, "fake-term")
	// The fake terminal records the argv it was handed, one element per line.
	if err := os.WriteFile(fake, []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > "+shSingleQuote(capPath)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	handler := filepath.Join(dir, "handler")
	if err := os.WriteFile(handler, []byte(resumeWrapperScript()), 0o755); err != nil {
		t.Fatal(err)
	}

	uuid := "11111111-2222-3333-4444-555555555555"
	run := func(uri string) (string, bool) {
		_ = os.Remove(capPath)
		cmd := exec.Command("bash", handler, uri)
		cmd.Env = append(os.Environ(), "CLAUDE_PR_RESUME_TERMINAL="+fake)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("wrapper failed: %v\n%s", err, out)
		}
		b, err := os.ReadFile(capPath)
		return string(b), err == nil
	}

	// A valid link resumes with the decoded cwd and CLAUDE_CONFIG_DIR.
	got, called := run("claude-resume://r/" + uuid + "/%2Fcfg/%2Fhome%2Fme%2Fp")
	if !called {
		t.Fatal("valid link did not invoke the terminal")
	}
	wantInner := "cd /home/me/p && CLAUDE_CONFIG_DIR=/cfg exec claude --resume " + uuid
	if !strings.Contains(got, "bash") || !strings.Contains(got, "-lc") || !strings.Contains(got, wantInner) {
		t.Errorf("terminal argv = %q, want to contain bash -lc %q", got, wantInner)
	}

	// An id with shell metacharacters is refused before anything is spawned.
	if _, called := run("claude-resume://r/" + uuid + ";touch$IFS/pwned/%2Fcfg/%2Fp"); called {
		t.Error("wrapper launched a terminal for a non-UUID id; injection guard failed")
	}
}

// TestResumeWrapperTerminalResolution proves the terminal is chosen at click
// time, in the documented precedence, rather than baked in at install: an
// explicit override wins; else an identifying env var picks the clicker; else it
// falls through to a terminal on PATH (so gnome-terminal, which exports nothing
// via the portal, is not hardwired to whatever was detected at install).
func TestResumeWrapperTerminalResolution(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	handler := filepath.Join(t.TempDir(), "handler")
	if err := os.WriteFile(handler, []byte(resumeWrapperScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	const validURI = "claude-resume://r/11111111-2222-3333-4444-555555555555/%2Fcfg/%2Fp"

	// stub writes an executable named `name` in dir that records "$0" + argv to
	// <name>.cap, and returns that capture path.
	stub := func(dir, name string) string {
		cap := filepath.Join(dir, name+".cap")
		// Absolute interpreter: PATH is restricted to the stub dir, so a
		// `/usr/bin/env bash` shebang couldn't resolve its interpreter.
		body := "#!/bin/sh\nprintf '%s\\n' \"$0\" \"$@\" > " + shSingleQuote(cap) + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return cap
	}
	// run the handler with an exact environment (PATH plus the given vars).
	run := func(pathDir string, env map[string]string) {
		cmd := exec.Command("bash", handler, validURI)
		cmd.Env = []string{"PATH=" + pathDir}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("wrapper failed: %v\n%s", err, out)
		}
	}
	// captured returns the recorded "$0" + argv lines, or nil if not invoked.
	captured := func(cap string) []string {
		b, err := os.ReadFile(cap)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}

	t.Run("override beats env marker", func(t *testing.T) {
		dir := t.TempDir()
		ghostty := stub(dir, "ghostty")
		override := stub(dir, "myterm")
		run(dir, map[string]string{
			"CLAUDE_PR_RESUME_TERMINAL": filepath.Join(dir, "myterm"),
			"TERM_PROGRAM":              "ghostty",
		})
		if captured(override) == nil {
			t.Error("override terminal was not used")
		}
		if captured(ghostty) != nil {
			t.Error("env-marker terminal used despite the override")
		}
	})

	t.Run("env marker picks the clicker (ghostty -e)", func(t *testing.T) {
		dir := t.TempDir()
		cap := stub(dir, "ghostty")
		run(dir, map[string]string{"TERM_PROGRAM": "ghostty"})
		got := captured(cap)
		if len(got) < 2 || filepath.Base(got[0]) != "ghostty" || got[1] != "-e" {
			t.Errorf("argv = %v, want ghostty invoked with -e", got)
		}
	})

	t.Run("no marker falls through to PATH, not a baked terminal", func(t *testing.T) {
		dir := t.TempDir()
		cap := stub(dir, "gnome-terminal") // the only terminal on PATH
		run(dir, nil)                      // no override, no identifying env
		got := captured(cap)
		if len(got) < 2 || filepath.Base(got[0]) != "gnome-terminal" || got[1] != "--" {
			t.Errorf("argv = %v, want gnome-terminal invoked with --", got)
		}
	})
}
