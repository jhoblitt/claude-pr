# claude-pr

Reverse-map GitHub pull requests to the [Claude Code](https://claude.com/claude-code)
sessions responsible for them, and see what PRs your live sessions are tracking.

When you run many parallel Claude Code sessions, it's easy to lose track of which
session opened a given PR — or what each running session is currently working on.
Claude Code already records the answer in each session's local JSONL transcript:
the `gh pr create` calls a session ran, and the PRs it references (`pr-link`
records). `claude-pr` reads those transcripts (read-only) to answer two questions:

- **Which session created PR #N?** — `claude-pr <N>`
- **What is every live session working on?** — `claude-pr` (no PR argument)

## Usage

```
claude-pr [flags] [<PR_NUMBER> [owner/repo]]
```

### PR mode — who created a PR

```
$ claude-pr 17801
CREATOR 2026-06-22T21:56:30.590Z  ci-loop-to-iscsi [724ceb21-...]  (-home-jhoblitt-github-rook7)
```

The CREATOR is the session that actually invoked `gh pr create` for that PR
(detected from the command, not a mention) and whose result carried the PR URL.
Sessions that only edited/viewed/referenced it are reported as `touched`.

- `-c`, `--creator` — print only the true creator.

### List mode — what live sessions are tracking

With no PR number, it lists the currently-live sessions (process still running,
from the daemon's `sessions/` registry) as `name · uuid · cwd · status`, each
followed by a tree of the PRs it tracks. Created PRs are flagged; each PR id is a
clickable terminal hyperlink ([OSC 8](https://gist.github.com/egmontkob/eb114294efbcd5adb1944c9f3cb5feda)).

```
$ claude-pr
ci-loop-to-iscsi                724ceb21  ~/github/rook7   idle
  └ #17801  (created)
```

Flags:

- `-a`, `--all` — also list sessions with no tracked PRs (hidden by default).
- `--exited` — also include exited (no longer running) sessions, shown with an
  `exited` status (live sessions only by default).
- `-c`, `--creator` — show only the PRs each session created.
- `--status` — annotate each PR with live GitHub state (OPEN/MERGED/CLOSED,
  draft, check counts, review decision) via the `gh` CLI.
- `--url` — print raw PR URLs instead of terminal hyperlinks.
- `--full-uuid` — show the full session UUID (default: 8-char prefix).
- `--color` / `--no-color` — force or disable ANSI color (default: auto; honors
  `NO_COLOR`).
- `--resume-links` / `--no-resume-links` — make each session name/uuid a
  clickable link that resumes it (see [Resuming in WezTerm](#resuming-in-wezterm)).
  Auto-enabled when running under WezTerm on a TTY.

## Resuming in WezTerm

`claude --resume` only accepts a full session UUID or the exact session title,
and it is scoped to the session's project directory — so the short ids in the
listing can't be pasted into it directly. Under [WezTerm](https://wezterm.org),
`claude-pr` instead makes each session's name and id a clickable hyperlink with a
custom `claude-resume://` scheme that carries the full UUID and cwd. This is
auto-enabled when WezTerm is detected (`$WEZTERM_PANE`) and stdout is a TTY.

For the click to resume the session, your WezTerm config needs an `open-uri`
handler that turns the link into `claude --resume`. Install it automatically:

```
claude-pr --install-wezterm
```

This creates `~/.wezterm.lua` if you don't have one, or injects a
marker-delimited handler block into your existing config (backing it up to
`*.claude-pr.bak` first). It's idempotent — re-running updates the block in
place. WezTerm auto-reloads on save.

Or add the handler yourself:

```lua
-- ~/.wezterm.lua
local wezterm = require 'wezterm'
local act = wezterm.action

local function urldecode(s)
  return (s:gsub('%%(%x%x)', function(h) return string.char(tonumber(h, 16)) end))
end
wezterm.on('open-uri', function(window, pane, uri)
  -- claude-resume://r/<id>/<urlencoded CLAUDE_CONFIG_DIR>/<urlencoded cwd>
  -- (path form: WezTerm won't click a ?query URI)
  local id, cfg, cwd = uri:match('^claude%-resume://r/([^/]+)/([^/]+)/([^/]+)$')
  if id then
    window:perform_action(act.SpawnCommandInNewTab {
      cwd = urldecode(cwd), -- cwd makes --resume's project scope match
      set_environment_variables = { CLAUDE_CONFIG_DIR = urldecode(cfg) },
      args = { 'bash', '-lc', 'exec claude --resume ' .. id },
    }, pane)
    return false -- handled; don't pass the unknown scheme to the OS opener
  end
end)
```

Then Ctrl+Click (WezTerm's default link trigger) a session in the list to open
`claude --resume` for it in a new tab. Without the handler the link is inert.

## Install

```
go install github.com/jhoblitt/claude-pr@latest
```

or build from source:

```
git clone https://github.com/jhoblitt/claude-pr
cd claude-pr
go build -o ~/bin/claude-pr .
```

## How it works

`claude-pr` reads Claude Code's session transcripts under `$CLAUDE_CONFIG_DIR`
(falling back to `~/.claude-personal`, `~/.claude`, or `~/.config/claude`). It
never writes to them. PR creation is identified by correlating a `gh pr create`
Bash tool call with the bare PR-URL line it printed; tracked PRs come from
`pr-link` records.

## Notes / caveats

- **Linux-specific liveness.** List mode decides which sessions are "live" by
  checking `/proc/<pid>`, so it must run where it can see the host process table.
- **`--status` needs `gh`** authenticated; without it, listing still works.
- The PR↔session creator match assumes a literal `gh pr create` invocation; a
  creator that wraps it behind a variable or unusual quoting may be missed.
