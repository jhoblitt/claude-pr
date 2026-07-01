package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	wezBegin = "-- >>> claude-pr resume handler (managed by `claude-pr --install-wezterm`) >>>"
	wezEnd   = "-- <<< claude-pr resume handler <<<"
)

// weztermBlock is the self-contained, marker-delimited open-uri handler that
// turns a claude-resume:// link into `claude --resume` in a new tab.
func weztermBlock() string {
	return wezBegin + "\n" + `do
  local wezterm = require 'wezterm'
  local act = wezterm.action
  local function urldecode(s)
    return (s:gsub('%%(%x%x)', function(h) return string.char(tonumber(h, 16)) end))
  end
  wezterm.on('open-uri', function(window, pane, uri)
    -- claude-resume://r/<id>/<urlencoded CLAUDE_CONFIG_DIR>/<urlencoded cwd>
    local id, cfg, cwd = uri:match('^claude%-resume://r/([^/]+)/([^/]+)/([^/]+)$')
    if id then
      wezterm.log_info('claude-pr: resume ' .. uri) -- visible in the debug overlay (Ctrl+Shift+L)
      window:perform_action(act.SpawnCommandInNewTab {
        cwd = urldecode(cwd), -- cwd makes --resume's project scope match
        set_environment_variables = { CLAUDE_CONFIG_DIR = urldecode(cfg) }, -- not set by the login shell
        args = { 'bash', '-lc', 'exec claude --resume ' .. id },
      }, pane)
      return false -- handled; don't pass the unknown scheme to the OS opener
    end
  end)
end` + "\n" + wezEnd
}

func freshWeztermConfig() string {
	return `-- WezTerm configuration -- https://wezterm.org
local wezterm = require 'wezterm'
local config = wezterm.config_builder()

` + weztermBlock() + `

return config
`
}

// weztermConfigPath picks the config to edit: $WEZTERM_CONFIG_FILE, else the
// first existing of ~/.config/wezterm/wezterm.lua or ~/.wezterm.lua, else
// ~/.wezterm.lua (to be created).
func weztermConfigPath() string {
	if p := os.Getenv("WEZTERM_CONFIG_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".wezterm.lua"
	}
	dotfile := filepath.Join(home, ".wezterm.lua")
	for _, c := range []string{filepath.Join(home, ".config", "wezterm", "wezterm.lua"), dotfile} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return dotfile
}

// injectBlock returns content with the handler block inserted/replaced, and the
// action taken. A marked block is replaced in place; otherwise the block is
// inserted before the last top-level `return` (so it runs before the chunk
// returns), or appended if there is none.
func injectBlock(content, block string) (string, string) {
	if i := strings.Index(content, wezBegin); i >= 0 {
		if j := strings.Index(content, wezEnd); j > i {
			return content[:i] + block + content[j+len(wezEnd):], "updated"
		}
	}
	lines := strings.Split(content, "\n")
	insertAt := -1
	for idx, ln := range lines {
		if strings.HasPrefix(ln, "return") {
			insertAt = idx
		}
	}
	if insertAt < 0 {
		sep := "\n"
		if strings.HasSuffix(content, "\n") || content == "" {
			sep = ""
		}
		return content + sep + "\n" + block + "\n", "added"
	}
	out := append([]string{}, lines[:insertAt]...)
	out = append(out, "", block, "")
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n"), "added"
}

func installWezterm() {
	path := weztermConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "claude-pr: "+err.Error())
			os.Exit(1)
		}
		if err := os.WriteFile(path, []byte(freshWeztermConfig()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "claude-pr: "+err.Error())
			os.Exit(1)
		}
		fmt.Println("claude-pr: created " + path + " with the resume handler.")
		fmt.Println("WezTerm auto-reloads its config; Ctrl+Click a session in `claude-pr` to resume it.")
		return
	}
	newContent, action := injectBlock(string(data), weztermBlock())
	if newContent == string(data) {
		fmt.Println("claude-pr: resume handler already up to date in " + path)
		return
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	backup := path + ".claude-pr.bak"
	if err := os.WriteFile(backup, data, mode); err != nil {
		fmt.Fprintln(os.Stderr, "claude-pr: could not write backup: "+err.Error())
		os.Exit(1)
	}
	if err := os.WriteFile(path, []byte(newContent), mode); err != nil {
		fmt.Fprintln(os.Stderr, "claude-pr: "+err.Error())
		os.Exit(1)
	}
	fmt.Printf("claude-pr: %s resume handler in %s (backup: %s)\n", action, path, backup)
	fmt.Println("WezTerm auto-reloads its config; Ctrl+Click a session in `claude-pr` to resume it.")
}
