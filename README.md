# claude-pr

[![CI](https://github.com/jhoblitt/claude-pr/actions/workflows/ci.yml/badge.svg)](https://github.com/jhoblitt/claude-pr/actions/workflows/ci.yml)

Reverse-map GitHub pull requests **and GitLab merge requests** to the
[Claude Code](https://claude.com/claude-code) sessions responsible for them, and
see what PRs/MRs your live sessions are tracking.

When you run many parallel Claude Code sessions, it's easy to lose track of which
session opened a given PR — or what each running session is currently working on.
Claude Code already records the answer in each session's local JSONL transcript:
the PRs/MRs it references (`pr-link` records — one record type covering GitHub
PRs and GitLab MRs alike), and the `gh pr create` calls a session ran.
`claude-pr` reads those transcripts (read-only) to answer two questions:

- **Which session created PR/MR #N?** — `claude-pr <N>`
- **What is every live session working on?** — `claude-pr` (no argument)

GitLab support keys off a separate config dir: point `CLAUDE_CONFIG_DIR` at the
one you use for an internal GitLab instance (see [How it works](#how-it-works)).

## Usage

```
claude-pr [flags] [<ref>]    # <ref>: 1234, #1234, !1234, or a PR/MR URL
```

### Reverse lookup — which sessions reference a PR/MR

Give a PR/MR as a bare number, `#`- or `!`-prefixed, or a full URL, and
`claude-pr` reports only the session(s) referencing it, in the same row format as
the list below:

```
$ claude-pr 17801
claude-pr         8ffe82dd  ~/github/rook7  busy
  └ rook/rook#17801

$ claude-pr 17801 --exited
ci-loop-to-iscsi  724ceb21  ~/github/rook7  exited
  └ rook/rook#17801  (created)
```

Accepts `1234`, `#1234` / `!1234` (quote it as `'#1234'` so the shell doesn't
treat it as a comment), a GitHub PR URL
(`https://github.com/<owner>/<repo>/pull/1234`), or a GitLab MR URL
(`https://<host>/<group>/<project>/-/merge_requests/1234`) — a URL also pins the
project. Live sessions only by default; add `--exited` to include exited ones,
and `-c`/`--creator` to show only sessions that *created* the PR. Adding
`-o`/`--open` reports the match only when that PR/MR is still open.

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
- `--status` — annotate each PR/MR with live state (OPEN/MERGED/CLOSED, draft,
  checks, and for GitHub the review decision) — via the `gh` CLI for GitHub PRs
  and the [`glab`](https://gitlab.com/gitlab-org/cli) CLI for GitLab MRs.
- `-o`, `--open` — keep only PRs/MRs that are OPEN (draft or not); implies
  `--status`. Merged, closed, and unresolved ones are dropped, along with any
  session left with none. Needs `gh` (for GitHub) and/or `glab` (for GitLab).
- `--url` — print raw PR URLs instead of terminal hyperlinks.
- `--full-uuid` — show the full session UUID (default: 8-char prefix).
- `--color` / `--no-color` — force or disable ANSI color (default: auto; honors
  `NO_COLOR`).
- `--resume-links` / `--no-resume-links` — make each session name/uuid a
  clickable link that resumes it (see [Resuming in WezTerm](#resuming-in-wezterm)).
  Auto-enabled when running under WezTerm on a TTY.

### Exit status

`0` normal output; `1` no match (a lookup found no session, or `--open`
filtered everything out) — handy in scripts, e.g.
`claude-pr -o 17801 && echo "still being worked on"`; `2` usage error.

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
place. WezTerm auto-reloads on save. Re-run it after upgrading `claude-pr`:
older handler blocks accepted a broader URI pattern than they should have.

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
  -- id is spliced into a shell command below, so its pattern must stay
  -- restricted to UUID characters — never widen it to [^/]+.
  local id, cfg, cwd = uri:match('^claude%-resume://r/([%w%-]+)/([^/]+)/([^/]+)$')
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
if that is set — and **only** there, so a separate config dir (e.g. one you use
for an internal GitLab instance) stays isolated from your default sessions —
otherwise it falls back to Claude Code's default `~/.claude`. It never writes to
them. Tracked PRs/MRs come from `pr-link`
records (a single record type Claude Code writes for GitHub PRs **and** GitLab
merge requests, carrying the real review URL); PR creation is additionally
identified by correlating a `gh pr create` Bash tool call with the bare PR-URL
line it printed.

Each PR/MR link uses the URL Claude Code recorded, so GitLab MRs (and
GitHub Enterprise PRs) point at their real host rather than github.com.

## Notes / caveats

- **Unix liveness.** List mode decides which sessions are "live" by probing
  pids with `kill(2)` (signal 0), which works on Linux and macOS — but it must
  run where it can see the session processes (not in a PID-namespaced sandbox).
- **`--status` needs `gh`** (GitHub) and/or **`glab`** (GitLab) authenticated —
  for a self-hosted GitLab instance, `glab` must have a token for that host
  (e.g. `glab auth login --hostname <host>`). Without the relevant CLI, those
  PRs/MRs simply show no status; the rest of the listing still works. Each fetch
  is bounded to 30s so a hung CLI can't wedge the listing.
- **`--open` and `--status` span providers.** GitLab MR state is normalized to
  the same OPEN/MERGED/CLOSED vocabulary, so `--open` filters GitHub PRs and
  GitLab MRs uniformly; GitLab CI shows as a single pipeline glyph (✓/✗/⧖)
  rather than GitHub's per-check counts.
- The **creator** flag (`(created)`) is currently GitHub-only: it correlates a
  literal `gh pr create` invocation with the bare PR-URL line it printed. GitLab
  MRs are still tracked (via `pr-link`), just never flagged `(created)`.
