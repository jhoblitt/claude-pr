package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The resume link claude-pr emits is an OSC 8 hyperlink with a custom
// claude-resume:// scheme. Terminals that don't script link clicks (Ghostty,
// kitty, foot, …) hand the URL to the OS opener (xdg-open/open) instead — so
// registering an OS-level handler for the scheme makes the links resume a
// session there, the same way the WezTerm open-uri hook does in-process.
const (
	resumeScheme    = "claude-resume"
	desktopFileName = "claude-pr-resume.desktop"
)

// installURLHandler registers a claude-resume:// handler with the OS. It is the
// terminal-agnostic counterpart to --install-wezterm: it works under any
// terminal that forwards an unhandled OSC 8 scheme to the system opener.
func installURLHandler() {
	switch runtime.GOOS {
	case "linux":
		installURLHandlerLinux()
	case "darwin":
		fmt.Println("claude-pr: --install-url-handler is not yet automated on macOS.")
		fmt.Println("macOS routes custom schemes through an app bundle (CFBundleURLTypes),")
		fmt.Println("not a config file; see the README for a manual claude-resume:// setup.")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "claude-pr: --install-url-handler supports Linux; see the README for other platforms.")
		os.Exit(1)
	}
}

// xdgDataHome is $XDG_DATA_HOME, or ~/.local/share per the XDG base-dir spec.
func xdgDataHome() string {
	if p := os.Getenv("XDG_DATA_HOME"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".local/share"
	}
	return filepath.Join(home, ".local", "share")
}

// urlHandlerInstalled reports whether the claude-resume:// desktop entry is in
// place, so resume links are live regardless of terminal. Used to auto-enable
// links the same way WezTerm detection does.
func urlHandlerInstalled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat(filepath.Join(xdgDataHome(), "applications", desktopFileName))
	return err == nil
}

func installURLHandlerLinux() {
	data := xdgDataHome()
	wrapperDir := filepath.Join(data, "claude-pr")
	wrapperPath := filepath.Join(wrapperDir, "claude-resume-handler")
	appsDir := filepath.Join(data, "applications")
	desktopPath := filepath.Join(appsDir, desktopFileName)

	launch := terminalLaunchArgv()
	for _, d := range []string{wrapperDir, appsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "claude-pr: "+err.Error())
			os.Exit(1)
		}
	}
	if err := os.WriteFile(wrapperPath, []byte(resumeWrapperScript(launch)), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "claude-pr: "+err.Error())
		os.Exit(1)
	}
	if err := os.WriteFile(desktopPath, []byte(desktopEntry(wrapperPath)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "claude-pr: "+err.Error())
		os.Exit(1)
	}

	// Make the desktop entry the default handler for the scheme, then refresh
	// the desktop database. Both are best-effort: the files above are what make
	// resume links work, and a distro may lack one of these tools.
	run := func(name string, args ...string) {
		if _, err := exec.LookPath(name); err != nil {
			return
		}
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			fmt.Printf("claude-pr: %s failed (%v): %s\n", name, err, strings.TrimSpace(string(out)))
		}
	}
	run("xdg-mime", "default", desktopFileName, "x-scheme-handler/"+resumeScheme)
	run("update-desktop-database", appsDir)

	fmt.Println("claude-pr: registered the claude-resume:// handler.")
	fmt.Printf("  wrapper: %s\n  desktop: %s\n  terminal: %s\n", wrapperPath, desktopPath, strings.Join(launch, " "))
	fmt.Println("Ctrl/Cmd+Click a session in `claude-pr` to resume it in a new terminal window.")
	fmt.Println("Override the terminal without reinstalling via $CLAUDE_PR_RESUME_TERMINAL.")
}

// terminalLaunchArgv is the argv prefix that runs a command in a new window of
// the user's terminal (e.g. {"ghostty","-e"}). The handler is spawned by
// xdg-open with no controlling terminal, so it must start one itself. The
// current terminal is detected from its env markers, falling back to the first
// known terminal on PATH.
func terminalLaunchArgv() []string {
	if s := os.Getenv("CLAUDE_PR_RESUME_TERMINAL"); s != "" {
		return strings.Fields(s)
	}
	switch {
	case os.Getenv("GHOSTTY_RESOURCES_DIR") != "" || os.Getenv("TERM_PROGRAM") == "ghostty":
		return []string{"ghostty", "-e"}
	case os.Getenv("WEZTERM_PANE") != "" || os.Getenv("TERM_PROGRAM") == "WezTerm":
		return []string{"wezterm", "start", "--"}
	case os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("TERM") == "xterm-kitty":
		return []string{"kitty"}
	}
	for _, t := range []struct {
		bin  string
		args []string
	}{
		{"ghostty", []string{"-e"}},
		{"wezterm", []string{"start", "--"}},
		{"kitty", nil},
		{"foot", nil},
		{"alacritty", []string{"-e"}},
		{"gnome-terminal", []string{"--"}},
		{"konsole", []string{"-e"}},
		{"xterm", []string{"-e"}},
	} {
		if _, err := exec.LookPath(t.bin); err == nil {
			return append([]string{t.bin}, t.args...)
		}
	}
	return []string{"xterm", "-e"}
}

// shSingleQuote wraps s in single quotes safe for POSIX sh, so an argv element
// embeds literally in the generated script regardless of its contents.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resumeWrapperScript is the bash handler xdg-open runs with the clicked URL. It
// mirrors the WezTerm open-uri block: parse claude-resume://r/<id>/<enc cfg>/<enc
// cwd>, keep <id> UUID-only (it is spliced into a command), then resume in a new
// terminal window with the recorded cwd and CLAUDE_CONFIG_DIR.
func resumeWrapperScript(launch []string) string {
	quoted := make([]string, len(launch))
	for i, a := range launch {
		quoted[i] = shSingleQuote(a)
	}
	return `#!/usr/bin/env bash
# claude-pr resume handler — managed by ` + "`claude-pr --install-url-handler`" + `.
# xdg-open runs this with a claude-resume://r/<id>/<enc cfg>/<enc cwd> URL and it
# opens ` + "`claude --resume <id>`" + ` in a new terminal window.
set -euo pipefail

uri=${1:-}
rest=${uri#claude-resume://r/}
[ "$rest" = "$uri" ] && exit 0 # not the scheme/path form we emit

id=${rest%%/*}; rest=${rest#*/}
cfg_enc=${rest%%/*}; rest=${rest#*/}
cwd_enc=${rest%%/*}

# id is spliced into the command below, so keep it UUID-only. Never widen this.
case $id in "" | *[!0-9A-Za-z-]*) exit 0 ;; esac

urldecode() { printf '%b' "${1//%/\\x}"; }
cfg=$(urldecode "$cfg_enc")
cwd=$(urldecode "$cwd_enc")
inner=$(printf 'cd %q && CLAUDE_CONFIG_DIR=%q exec claude --resume %q' "$cwd" "$cfg" "$id")

term=(` + strings.Join(quoted, " ") + `)
[ -n "${CLAUDE_PR_RESUME_TERMINAL:-}" ] && read -r -a term <<<"$CLAUDE_PR_RESUME_TERMINAL"
exec "${term[@]}" bash -lc "$inner"
`
}

// desktopEntry is the XDG desktop entry that binds the scheme to the wrapper.
// NoDisplay keeps it out of application menus; it exists only as a URL handler.
func desktopEntry(wrapperPath string) string {
	return `[Desktop Entry]
Type=Application
Name=claude-pr resume handler
Comment=Resume a Claude Code session from a claude-resume:// link
Exec="` + wrapperPath + `" %u
Terminal=false
NoDisplay=true
MimeType=x-scheme-handler/` + resumeScheme + `;
`
}
