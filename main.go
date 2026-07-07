// claude-pr maps a GitHub PR to the Claude Code session(s) that reference it,
// and lists the live sessions and the PRs each is tracking.
//
// Given a PR reference — bare "1234", "#1234", or a PR URL — it reports only the
// session(s) whose tracked PRs (from "pr-link" records) include that PR. A PR is
// flagged "(created)" for a session when that session actually invoked
// `gh pr create` for it (detected from the command, not a mention) and its
// result carried the bare PR URL.
//
// With no PR argument, it lists the currently-live sessions (from the daemon's
// sessions/ registry, process still running) as aligned columns —
// name · uuid · cwd · status — each followed by a tree of the PRs it is
// tracking, with created PRs flagged and each PR id a clickable terminal
// hyperlink (OSC 8). Session identity shows the /rename title when set, else
// the UUID; output is colored on a TTY (honoring NO_COLOR).
//
// Run `claude-pr --help` for flags and examples; the README covers the
// resume-link integration (WezTerm's open-uri hook, or an OS-level
// claude-resume:// handler for other terminals).
package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// rePRArgURL parses a full GitHub PR URL passed as the CLI argument.
var rePRArgURL = regexp.MustCompile(`^https?://github\.com/([^/\s]+/[^/\s]+)/pull/(\d+)(?:[/?#].*)?$`)

// prQuery filters the listing to sessions referencing a specific PR.
type prQuery struct {
	repo string // "" matches any owner/repo
	num  int
}

// parsePRArg parses a PR reference: bare "1234", "#1234", or a full PR URL.
// repo is "" unless a URL supplied one. PR numbers start at 1.
func parsePRArg(s string) (repo string, num int, ok bool) {
	s = strings.TrimSpace(s)
	if m := rePRArgURL.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[2]); err == nil && n > 0 {
			return m[1], n, true
		}
		return "", 0, false
	}
	s = strings.TrimPrefix(s, "#")
	if s == "" {
		return "", 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", 0, false
		}
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return "", n, true
	}
	return "", 0, false
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func printUsage() {
	fmt.Print(`claude-pr — map GitHub PRs to the Claude Code sessions responsible for them.

Usage:
  claude-pr [flags] [<PR>]            # <PR>: 1234, #1234, or a github.com PR URL

Modes:
  with a PR ref      report only the session(s) referencing that PR (live by
                     default; add --exited to include exited sessions).
  no arguments       list currently-live sessions and the PRs each is tracking.

Flags:
  -c, --creator    show only PRs/sessions where the session created the PR.
  -a, --all        list mode: also show sessions with no tracked PRs.
      --exited     also include exited (no longer running) sessions, shown
                   with an "exited" status.
      --status     annotate each PR with live GitHub state (OPEN/MERGED/
                   CLOSED, draft, checks, review) via gh.
  -o, --open       keep only OPEN PRs (draft or not); implies --status. Drops
                   merged, closed, and unresolved PRs, and any session left
                   with no open PR. Needs the gh CLI.
      --url        print raw PR URLs instead of terminal hyperlinks.
      --full-uuid  show the full session UUID (default: 8-char prefix).
      --color      force ANSI color.
      --no-color   disable ANSI color (default: auto; honors NO_COLOR).
      --resume-links / --no-resume-links
                   make each session name/uuid a clickable link that resumes it.
                   Auto-on on a TTY under WezTerm, or once a claude-resume://
                   handler is installed (see --install-wezterm /
                   --install-url-handler).
      --install-wezterm
                   create or update ~/.wezterm.lua with the open-uri handler
                   that makes the resume links work, then exit.
      --install-url-handler
                   register an OS-level claude-resume:// handler (Linux
                   xdg-mime) so resume links work in Ghostty, kitty, and any
                   terminal that defers unknown schemes to the system opener,
                   then exit.
  -h, --help       show this help and exit.

Examples:
  claude-pr 17801             live sessions referencing PR #17801
  claude-pr '#17801'          same; quote the # so the shell keeps it
  claude-pr <pr-url>          match the exact owner/repo + PR from a URL
  claude-pr 17801 --exited    include exited sessions too
  claude-pr -c 17801          only sessions that created it
  claude-pr                   all live sessions and the PRs they track
  claude-pr -o                only sessions with an open PR (draft or not)

Exit status:
  0  normal output (list rendered, or the lookup matched)
  1  no match: the lookup found no session, or --open filtered everything out
  2  usage error

Sessions are read read-only from $CLAUDE_CONFIG_DIR if set (and only there, so a
separate config for e.g. an internal GitLab instance stays isolated), else from
Claude Code's default ~/.claude. Liveness is probed with kill(2), so it needs to
run where it can see the session processes; --status needs the gh CLI
authenticated.
`)
}

func main() {
	creatorOnly := false
	showStatus := false
	showEmpty := false
	includeExited := false
	openOnly := false
	forceURL := false
	colorMode := "auto"
	resumeMode := "auto"
	var pos []string
	for _, a := range os.Args[1:] {
		switch a {
		case "-h", "--help":
			printUsage()
			return
		case "--install-wezterm":
			installWezterm()
			return
		case "--install-url-handler":
			installURLHandler()
			return
		case "-c", "--creator":
			creatorOnly = true
		case "-a", "--all":
			showEmpty = true
		case "--exited":
			includeExited = true
		case "--status":
			showStatus = true
		case "-o", "--open":
			openOnly = true
		case "--url":
			forceURL = true
		case "--full-uuid":
			fullUUID = true
		case "--color":
			colorMode = "always"
		case "--no-color":
			colorMode = "never"
		case "--resume-links":
			resumeMode = "always"
		case "--no-resume-links":
			resumeMode = "never"
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				fmt.Fprintln(os.Stderr, "claude-pr: unknown flag: "+a+" (see --help)")
				os.Exit(2)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) > 1 {
		fmt.Fprintln(os.Stderr, "claude-pr: expected at most one PR reference, got: "+strings.Join(pos, " "))
		os.Exit(2)
	}
	tty := isTTY(os.Stdout)
	switch colorMode {
	case "always":
		colorEnabled = true
	case "never":
		colorEnabled = false
	default:
		colorEnabled = tty && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	useHyperlink = tty && !forceURL
	showRawURL = forceURL || !tty
	switch resumeMode {
	case "always":
		resumeLinks = true
	case "never":
		resumeLinks = false
	default: // auto: on a TTY when a resume handler is likely present — WezTerm's
		// open-uri hook, or an installed claude-resume:// URL handler.
		wez := os.Getenv("WEZTERM_PANE") != "" || os.Getenv("TERM_PROGRAM") == "WezTerm"
		resumeLinks = tty && !forceURL && (wez || urlHandlerInstalled())
	}

	roots := discoverRoots()
	if len(roots) == 0 {
		fmt.Fprintln(os.Stderr, "claude-pr: no Claude Code projects/ dir found (set CLAUDE_CONFIG_DIR)")
		os.Exit(2)
	}

	if openOnly {
		showStatus = true // --open needs the live state it filters on
	}

	var filter *prQuery
	if len(pos) > 0 {
		repo, num, ok := parsePRArg(pos[0])
		if !ok {
			fmt.Fprintln(os.Stderr, "claude-pr: not a PR reference (use 1234, #1234, or a PR URL): "+pos[0])
			os.Exit(2)
		}
		filter = &prQuery{repo: repo, num: num}
	}
	if code := runListMode(creatorOnly, showStatus, showEmpty, includeExited, openOnly, filter, roots); code != 0 {
		os.Exit(code)
	}
}
