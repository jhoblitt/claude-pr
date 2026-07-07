package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// detectEnv clears every terminal-detection variable, then applies overrides,
// so a test's expectation doesn't depend on the host terminal.
func detectEnv(t *testing.T, set map[string]string) {
	for _, k := range []string{
		"CLAUDE_PR_RESUME_TERMINAL", "GHOSTTY_RESOURCES_DIR",
		"TERM_PROGRAM", "WEZTERM_PANE", "KITTY_WINDOW_ID", "TERM",
	} {
		t.Setenv(k, "")
	}
	for k, v := range set {
		t.Setenv(k, v)
	}
}

func TestTerminalLaunchArgv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"override", map[string]string{"CLAUDE_PR_RESUME_TERMINAL": "kitty --hold"}, []string{"kitty", "--hold"}},
		{"ghostty-resources", map[string]string{"GHOSTTY_RESOURCES_DIR": "/x"}, []string{"ghostty", "-e"}},
		{"ghostty-termprog", map[string]string{"TERM_PROGRAM": "ghostty"}, []string{"ghostty", "-e"}},
		{"wezterm", map[string]string{"WEZTERM_PANE": "0"}, []string{"wezterm", "start", "--"}},
		{"kitty", map[string]string{"KITTY_WINDOW_ID": "1"}, []string{"kitty"}},
		{"kitty-term", map[string]string{"TERM": "xterm-kitty"}, []string{"kitty"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			detectEnv(t, c.env)
			got := terminalLaunchArgv()
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("terminalLaunchArgv() = %v, want %v", got, c.want)
			}
		})
	}
}

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
	if err := os.WriteFile(path, []byte(resumeWrapperScript([]string{"ghostty", "-e"})), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n rejected the wrapper: %v\n%s", err, out)
	}
}

// TestResumeWrapperEndToEnd drives the generated wrapper with a fake terminal
// and asserts what command it would launch — the real proof that the URL is
// parsed, decoded, and (for a bad id) refused.
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
	if err := os.WriteFile(handler, []byte(resumeWrapperScript([]string{fake})), 0o755); err != nil {
		t.Fatal(err)
	}

	uuid := "11111111-2222-3333-4444-555555555555"
	run := func(uri string) (string, bool) {
		_ = os.Remove(capPath)
		if out, err := exec.Command("bash", handler, uri).CombinedOutput(); err != nil {
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
