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
// name · uuid · cwd · status — each followed by a tree of the PRs it is tracking
// (from "pr-link" records), with created PRs flagged. Each PR id is a clickable
// terminal hyperlink (OSC 8) to the PR. Session identity shows the /rename title
// when set, else the UUID; output is colored on a TTY (honoring NO_COLOR);
// unnamed sessions show "(unnamed)".
//
// Usage: claude-pr [flags] [<PR>]   (<PR>: 1234, #1234, or a github.com PR URL)
//
//	-c / --creator   show only PRs/sessions where the session created the PR.
//	-a / --all       list mode: also list sessions with no tracked PRs.
//	--exited         report/list exited (no-longer-running) sessions too,
//	                 shown with an "exited" status.
//	--status         annotate each PR with live GitHub state (OPEN/MERGED/
//	                 CLOSED, draft, checks, review) via `gh`.
//	-o / --open      keep only OPEN PRs (draft or not); implies --status.
//	--url            print raw PR URLs instead of terminal hyperlinks.
//	--full-uuid      show the full session UUID (default: 8-char prefix).
//	--color/--no-color  force or disable ANSI color (default: auto).
//	--resume-links/--no-resume-links  make each session name/uuid a
//	                 clickable resume link (auto-on under WezTerm on a TTY;
//	                 needs an open-uri handler in the wezterm config).
//	-h / --help      show usage and exit.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type record struct {
	Type         string `json:"type"`
	CustomTitle  string `json:"customTitle"`
	Timestamp    string `json:"timestamp"`
	PrNumber     int    `json:"prNumber"`
	PrRepository string `json:"prRepository"`
	Message      *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentItem struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	ID    string `json:"id"`
	Input *struct {
		Command string `json:"command"`
	} `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// reCreate matches an actual `gh pr create` invocation: at a command position
// (start of line, or after a shell separator), not buried inside a quoted string.
var reCreate = regexp.MustCompile("(?m)(^|[;&|(){}\n`])[ \t]*gh pr create([ \t]|$)")

// rePRURL captures (owner/repo, number) from a GitHub PR URL alone on a line.
var rePRURL = regexp.MustCompile(`(?m)^https?://github\.com/([^/\s]+/[^/\s]+)/pull/(\d+)[ \t]*\r?$`)

// rePRArgURL parses a full GitHub PR URL passed as the CLI argument.
var rePRArgURL = regexp.MustCompile(`^https?://github\.com/([^/\s]+/[^/\s]+)/pull/(\d+)(?:[/?#].*)?$`)

// prQuery filters the listing to sessions referencing a specific PR.
type prQuery struct {
	repo string // "" matches any owner/repo
	num  int
}

// parsePRArg parses a PR reference: bare "1234", "#1234", or a full PR URL.
// repo is "" unless a URL supplied one.
func parsePRArg(s string) (repo string, num int, ok bool) {
	s = strings.TrimSpace(s)
	if m := rePRArgURL.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[2])
		return m[1], n, err == nil
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
	n, err := strconv.Atoi(s)
	return "", n, err == nil
}

var (
	colorEnabled bool
	useHyperlink bool
	showRawURL   bool
	fullUUID     bool
	resumeLinks  bool
)

const (
	cReset     = "\033[0m"
	cBold      = "\033[1m"
	cDim       = "\033[2m"
	cUnderline = "\033[4m"
	cRed       = "\033[31m"
	cGreen     = "\033[32m"
	cYellow    = "\033[33m"
	cBlue      = "\033[34m"
	cMagenta   = "\033[35m"
	cCyan      = "\033[36m"
)

func uuidDisp(u string) string {
	if fullUUID || len(u) < 8 {
		return u
	}
	return u[:8]
}

// osc8 wraps text in an OSC 8 terminal hyperlink. The escape sequences are
// zero-width, so visible length == len(text).
func osc8(text, url string) string {
	return "\033]8;;" + url + "\033\\" + text + "\033]8;;\033\\"
}

// link wraps text in a hyperlink when PR hyperlinks are enabled, else as-is.
func link(text, url string) string {
	if !useHyperlink {
		return text
	}
	return osc8(text, url)
}

// pctEncode percent-encodes all but RFC 3986 unreserved bytes, so an absolute
// path becomes a single slash-free URI path segment.
func pctEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// cfgFromPath returns the Claude config dir for a transcript path (the part
// before "/projects/"), which `claude --resume` needs as CLAUDE_CONFIG_DIR.
func cfgFromPath(transcriptPath string) string {
	if i := strings.Index(transcriptPath, "/projects/"); i >= 0 {
		return transcriptPath[:i]
	}
	return ""
}

// resumeURI is the custom-scheme target a WezTerm open-uri handler turns into
// `claude --resume <id>`. A query-free path form is used because WezTerm won't
// make a "?...&..." URI clickable; cfg and cwd are URL-encoded path segments so
// the handler can set CLAUDE_CONFIG_DIR (absent from the spawn's login shell)
// and the project-scoping cwd: claude-resume://r/<id>/<enc cfg>/<enc cwd>.
func resumeURI(uuid, cfg, cwdAbs string) string {
	return "claude-resume://r/" + uuid + "/" + pctEncode(cfg) + "/" + pctEncode(cwdAbs)
}

func col(style, s string) string {
	if !colorEnabled || style == "" {
		return s
	}
	return style + s + cReset
}

// field left-pads s to visible width w (measured plain), then styles the cell.
func field(s string, w int, style string) string {
	if pad := w - len([]rune(s)); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return col(style, s)
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// statusRank orders sessions by how much they want attention.
func statusRank(s string) int {
	switch s {
	case "waiting":
		return 0
	case "busy":
		return 1
	case "idle":
		return 2
	case "shell":
		return 3
	case "exited":
		return 5
	default:
		return 4
	}
}

func statusStyle(s string) string {
	switch s {
	case "waiting":
		return cBold + cMagenta
	case "busy":
		return cYellow
	case "idle":
		return cGreen
	case "shell":
		return cBlue
	case "exited":
		return cDim
	default:
		return cDim
	}
}

// colorizePRStatus tints the tokens of a "[...]" PR-status annotation.
func colorizePRStatus(s string) string {
	if !colorEnabled {
		return s
	}
	toks := strings.Fields(s)
	for i, t := range toks {
		switch {
		case t == "OPEN", t == "APPROVED":
			toks[i] = col(cGreen, t)
		case t == "MERGED":
			toks[i] = col(cMagenta, t)
		case t == "CLOSED", t == "CHANGES_REQUESTED", t == "status?":
			toks[i] = col(cRed, t)
		case t == "draft":
			toks[i] = col(cDim, t)
		case t == "REVIEW_REQUIRED":
			toks[i] = col(cYellow, t)
		case strings.HasPrefix(t, "✓"):
			toks[i] = col(cGreen, t)
		case strings.HasPrefix(t, "✗"):
			toks[i] = col(cRed, t)
		case strings.HasPrefix(t, "⧖"):
			toks[i] = col(cYellow, t)
		}
	}
	return strings.Join(toks, " ")
}

// resultText flattens a tool_result's content (a JSON string, or an array of
// {type,text} blocks).
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, e := range blocks {
			b.WriteString(e.Text)
		}
		return b.String()
	}
	return ""
}

// latestCustomTitle returns the last /rename title in a transcript, or "".
func latestCustomTitle(data []byte) string {
	title := ""
	marker := []byte(`"custom-title"`)
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if !bytes.Contains(raw, marker) {
			continue
		}
		var rec record
		if json.Unmarshal(bytes.TrimSpace(raw), &rec) == nil && rec.Type == "custom-title" && rec.CustomTitle != "" {
			title = rec.CustomTitle
		}
	}
	return title
}

func discoverRoots() []string {
	var cands []string
	if c := os.Getenv("CLAUDE_CONFIG_DIR"); c != "" {
		cands = append(cands, filepath.Join(c, "projects"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{".claude-personal/projects", ".claude/projects", ".config/claude/projects"} {
			cands = append(cands, filepath.Join(home, d))
		}
	}
	seen := map[string]bool{}
	var roots []string
	for _, c := range cands {
		abs, err := filepath.Abs(c)
		if err != nil {
			abs = c
		}
		if seen[abs] {
			continue
		}
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			seen[abs] = true
			roots = append(roots, abs)
		}
	}
	return roots
}

// --- list mode (no PR argument) ---

type regSession struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Cwd       string `json:"cwd"`
	Pid       int    `json:"pid"`
	Alive     bool   `json:"-"`
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// readSessions returns the running sessions from each config's sessions/
// registry (the daemon prunes the registry on exit, so this is live-only),
// de-duplicated by session ID.
func readSessions(roots []string) []regSession {
	bySID := map[string]regSession{}
	seenDir := map[string]bool{}
	for _, root := range roots {
		dir := filepath.Join(filepath.Dir(root), "sessions")
		if seenDir[dir] {
			continue
		}
		seenDir[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var s regSession
			if json.Unmarshal(data, &s) != nil || s.SessionID == "" || !pidAlive(s.Pid) {
				continue
			}
			s.Alive = true
			if _, ok := bySID[s.SessionID]; !ok {
				bySID[s.SessionID] = s
			}
		}
	}
	out := make([]regSession, 0, len(bySID))
	for _, s := range bySID {
		out = append(out, s)
	}
	return out
}

// cwdFromTranscript returns the session's working directory, read from the first
// transcript record that carries one.
func cwdFromTranscript(data []byte) string {
	marker := []byte(`"cwd"`)
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if !bytes.Contains(raw, marker) {
			continue
		}
		var r struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(bytes.TrimSpace(raw), &r) == nil && r.Cwd != "" {
			return r.Cwd
		}
	}
	return ""
}

// transcriptIndex maps a session UUID to its top-level transcript path.
func transcriptIndex(roots []string) map[string]string {
	idx := map[string]string{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") || strings.Contains(p, "/subagents/") {
				return nil
			}
			uuid := strings.TrimSuffix(filepath.Base(p), ".jsonl")
			if _, ok := idx[uuid]; !ok {
				idx[uuid] = p
			}
			return nil
		})
	}
	return idx
}

type prRef struct {
	repo    string
	num     int
	created bool
}

// scanTracked returns the PRs a session references via "pr-link" records (its
// tracked set), unioned with PRs it created (a bare PR-URL line emitted by one
// of its `gh pr create` calls), with created ones flagged.
func scanTracked(data []byte) []prRef {
	cmdByID := map[string]string{}
	type res struct{ tuid, txt string }
	var results []res
	tracked := map[string]bool{}
	created := map[string]bool{}

	for _, raw := range bytes.Split(data, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var rec record
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		if rec.Type == "pr-link" && rec.PrRepository != "" && rec.PrNumber != 0 {
			tracked[rec.PrRepository+"#"+strconv.Itoa(rec.PrNumber)] = true
		}
		if rec.Message == nil {
			continue
		}
		var items []contentItem
		if json.Unmarshal(rec.Message.Content, &items) != nil {
			continue
		}
		for _, it := range items {
			switch it.Type {
			case "tool_use":
				if it.Name == "Bash" && it.ID != "" && it.Input != nil {
					cmdByID[it.ID] = it.Input.Command
				}
			case "tool_result":
				txt := resultText(it.Content)
				if strings.Contains(txt, "/pull/") {
					results = append(results, res{it.ToolUseID, txt})
				}
			}
		}
	}
	for _, r := range results {
		isCreate := reCreate.MatchString(cmdByID[r.tuid])
		if !isCreate {
			continue
		}
		for _, m := range rePRURL.FindAllStringSubmatch(r.txt, -1) {
			created[m[1]+"#"+m[2]] = true
		}
	}

	keys := map[string]bool{}
	for k := range tracked {
		keys[k] = true
	}
	for k := range created {
		keys[k] = true
	}
	refs := make([]prRef, 0, len(keys))
	for k := range keys {
		i := strings.LastIndex(k, "#")
		num, _ := strconv.Atoi(k[i+1:])
		refs = append(refs, prRef{repo: k[:i], num: num, created: created[k]})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].repo != refs[j].repo {
			return refs[i].repo < refs[j].repo
		}
		return refs[i].num < refs[j].num
	})
	return refs
}

func abbrevHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+"/") {
			return "~" + p[len(home):]
		}
	}
	return p
}

// fetchPRStatus returns a compact live status for a PR via `gh`, e.g.
// "OPEN draft ✓57 ✗1 REVIEW_REQUIRED" or "MERGED ✓58", or "status? <reason>".
func fetchPRStatus(repo string, num int) string {
	cmd := exec.Command("gh", "pr", "view", strconv.Itoa(num), "-R", repo,
		"--json", "state,isDraft,reviewDecision,statusCheckRollup")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return "status? " + msg
	}
	var v struct {
		State             string `json:"state"`
		IsDraft           bool   `json:"isDraft"`
		ReviewDecision    string `json:"reviewDecision"`
		StatusCheckRollup []struct {
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if json.Unmarshal(out.Bytes(), &v) != nil {
		return "status? parse error"
	}
	parts := []string{v.State}
	if v.State == "OPEN" && v.IsDraft {
		parts = append(parts, "draft")
	}
	var pass, fail, pend int
	for _, c := range v.StatusCheckRollup {
		s := c.Conclusion
		if s == "" {
			s = c.State
		}
		switch s {
		case "SUCCESS":
			pass++
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			fail++
		case "PENDING", "IN_PROGRESS", "QUEUED", "WAITING", "EXPECTED", "":
			pend++
		}
	}
	if pass > 0 {
		parts = append(parts, "✓"+strconv.Itoa(pass))
	}
	if fail > 0 {
		parts = append(parts, "✗"+strconv.Itoa(fail))
	}
	if pend > 0 {
		parts = append(parts, "⧖"+strconv.Itoa(pend))
	}
	if v.State == "OPEN" && v.ReviewDecision != "" {
		parts = append(parts, v.ReviewDecision)
	}
	return strings.Join(parts, " ")
}

// fetchStatuses resolves status for each distinct PR concurrently (bounded).
func fetchStatuses(prs []prRef) map[string]string {
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, p := range prs {
		wg.Add(1)
		sem <- struct{}{}
		go func(p prRef) {
			defer wg.Done()
			defer func() { <-sem }()
			s := fetchPRStatus(p.repo, p.num)
			mu.Lock()
			out[fmt.Sprintf("%s#%d", p.repo, p.num)] = s
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

// isOpenStatus reports whether a fetchPRStatus string denotes an OPEN PR (draft
// or not). The state is the leading token; drafts render as "OPEN draft …", so a
// prefix test excludes MERGED/CLOSED and the "status? …" error form.
func isOpenStatus(s string) bool {
	return s == "OPEN" || strings.HasPrefix(s, "OPEN ")
}

func runListMode(creatorOnly, showStatus, showEmpty, includeExited, openOnly bool, filter *prQuery, roots []string) {
	idx := transcriptIndex(roots)
	sessions := readSessions(roots)
	if includeExited {
		liveSet := map[string]bool{}
		for _, s := range sessions {
			liveSet[s.SessionID] = true
		}
		for uuid := range idx {
			if !liveSet[uuid] {
				sessions = append(sessions, regSession{SessionID: uuid}) // Alive=false => exited
			}
		}
	}

	type row struct {
		name, uuid, cwdAbs, cfg, status string
		prs                             []prRef
	}
	var rows []row
	for _, s := range sessions {
		var prs []prRef
		name, cwd := s.Name, s.Cwd
		tf := idx[s.SessionID]
		if tf != "" {
			if data, err := os.ReadFile(tf); err == nil {
				prs = scanTracked(data)
				if !s.Alive { // exited: registry is gone, so pull metadata from the transcript
					name, cwd = latestCustomTitle(data), cwdFromTranscript(data)
				}
			}
		}
		cfg := cfgFromPath(tf)
		if creatorOnly {
			var only []prRef
			for _, p := range prs {
				if p.created {
					only = append(only, p)
				}
			}
			prs = only
		}
		if filter != nil {
			var matched []prRef
			for _, p := range prs {
				if p.num == filter.num && (filter.repo == "" || p.repo == filter.repo) {
					matched = append(matched, p)
				}
			}
			prs = matched
			if len(prs) == 0 {
				continue
			}
		} else if len(prs) == 0 && !showEmpty {
			continue
		}
		st := s.Status
		if !s.Alive {
			st = "exited"
		} else if st == "" {
			st = "?"
		}
		rows = append(rows, row{name, s.SessionID, cwd, cfg, st, prs})
	}

	const unnamed = "(unnamed)"
	nameOf := func(r row) string {
		if r.name != "" {
			return r.name
		}
		return unnamed
	}
	sort.Slice(rows, func(i, j int) bool {
		if ri, rj := statusRank(rows[i].status), statusRank(rows[j].status); ri != rj {
			return ri < rj
		}
		if ui, uj := rows[i].name == "", rows[j].name == ""; ui != uj {
			return !ui // named sessions before unnamed
		}
		if ni, nj := nameOf(rows[i]), nameOf(rows[j]); ni != nj {
			return ni < nj
		}
		return rows[i].uuid < rows[j].uuid
	})

	if filter != nil && len(rows) == 0 {
		hint := ""
		if !includeExited {
			hint = " (use --exited to include exited sessions)"
		}
		fmt.Fprintln(os.Stderr, "claude-pr: no session references that PR"+hint)
		return
	}

	statusByKey := map[string]string{}
	if showStatus {
		if _, err := exec.LookPath("gh"); err != nil {
			if openOnly {
				fmt.Fprintln(os.Stderr, "claude-pr: --open needs the 'gh' CLI on PATH to determine PR state")
				os.Exit(1)
			}
			fmt.Fprintln(os.Stderr, "claude-pr: --status needs the 'gh' CLI on PATH; skipping status")
		} else {
			seen := map[string]bool{}
			var todo []prRef
			for _, r := range rows {
				for _, p := range r.prs {
					k := fmt.Sprintf("%s#%d", p.repo, p.num)
					if !seen[k] {
						seen[k] = true
						todo = append(todo, p)
					}
				}
			}
			statusByKey = fetchStatuses(todo)
		}
	}

	if openOnly {
		var kept []row
		for _, r := range rows {
			var open []prRef
			for _, p := range r.prs {
				if isOpenStatus(statusByKey[fmt.Sprintf("%s#%d", p.repo, p.num)]) {
					open = append(open, p)
				}
			}
			if len(open) > 0 {
				r.prs = open
				kept = append(kept, r)
			}
		}
		rows = kept
	}

	if openOnly && len(rows) == 0 {
		if filter != nil {
			fmt.Fprintln(os.Stderr, "claude-pr: that PR is not open")
		} else {
			hint := ""
			if !includeExited {
				hint = " (use --exited to include exited sessions)"
			}
			fmt.Fprintln(os.Stderr, "claude-pr: no session has an open tracked PR"+hint)
		}
		return
	}

	repos := map[string]bool{}
	for _, r := range rows {
		for _, p := range r.prs {
			repos[p.repo] = true
		}
	}
	singleRepo := len(repos) == 1
	refText := func(p prRef) string {
		if singleRepo {
			return "#" + strconv.Itoa(p.num)
		}
		return fmt.Sprintf("%s#%d", p.repo, p.num)
	}
	prURL := func(p prRef) string {
		return fmt.Sprintf("https://github.com/%s/pull/%d", p.repo, p.num)
	}

	wName, wUUID, wCwd, wRef := len(unnamed), 0, 0, 0
	for _, r := range rows {
		if n := len(nameOf(r)); n > wName {
			wName = n
		}
		if n := len(uuidDisp(r.uuid)); n > wUUID {
			wUUID = n
		}
		if n := len(abbrevHome(r.cwdAbs)); n > wCwd {
			wCwd = n
		}
		for _, p := range r.prs {
			if n := len(refText(p)); n > wRef {
				wRef = n
			}
		}
	}

	for i, r := range rows {
		if i > 0 {
			fmt.Println()
		}
		nameStyle := cBold
		if r.name == "" {
			nameStyle = cDim
		}
		nameText, uuidText := nameOf(r), uuidDisp(r.uuid)
		nameCell, uuidCell := col(nameStyle, nameText), col(cDim, uuidText)
		if resumeLinks {
			uri := resumeURI(r.uuid, r.cfg, r.cwdAbs)
			nameCell, uuidCell = osc8(nameCell, uri), osc8(uuidCell, uri)
		}
		nameCell += strings.Repeat(" ", wName-len(nameText))
		uuidCell += strings.Repeat(" ", wUUID-len(uuidText))
		fmt.Printf("%s  %s  %s  %s\n",
			nameCell,
			uuidCell,
			field(abbrevHome(r.cwdAbs), wCwd, cCyan),
			col(statusStyle(r.status), r.status))
		for j, p := range r.prs {
			conn := "├"
			if j == len(r.prs)-1 {
				conn = "└"
			}
			rt := refText(p)
			refStyle := cCyan
			if useHyperlink {
				refStyle = cCyan + cUnderline
			}
			ref := link(col(refStyle, rt), prURL(p))
			if pad := wRef - len(rt); pad > 0 {
				ref += strings.Repeat(" ", pad)
			}
			line := "  " + col(cDim, conn) + " " + ref
			if showRawURL {
				line += "  " + col(cDim, prURL(p))
			}
			if st := statusByKey[fmt.Sprintf("%s#%d", p.repo, p.num)]; st != "" {
				line += "  [" + colorizePRStatus(st) + "]"
			}
			if p.created {
				line += "  " + col(cGreen, "(created)")
			}
			fmt.Println(line)
		}
	}
}

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
      --exited     list mode: also include exited (no longer running) sessions,
                   shown with an "exited" status.
      --status     list mode: annotate each PR with live GitHub state
                   (OPEN/MERGED/CLOSED, draft, checks, review) via gh.
  -o, --open       keep only OPEN PRs (draft or not); implies --status. Drops
                   merged, closed, and unresolved PRs, and any session left
                   with no open PR. Needs the gh CLI.
      --url        print raw PR URLs instead of terminal hyperlinks.
      --full-uuid  show the full session UUID (default: 8-char prefix).
      --color      force ANSI color.
      --no-color   disable ANSI color (default: auto; honors NO_COLOR).
      --resume-links / --no-resume-links
                   make each session name/uuid a clickable link that resumes it.
                   Auto-on under WezTerm on a TTY; needs an open-uri handler in
                   your wezterm config (see --install-wezterm).
      --install-wezterm
                   create or update ~/.wezterm.lua with the open-uri handler
                   that makes the resume links work, then exit.
  -h, --help       show this help and exit.

Examples:
  claude-pr 17801             live sessions referencing PR #17801
  claude-pr '#17801'          same; quote the # so the shell keeps it
  claude-pr <pr-url>          match the exact owner/repo + PR from a URL
  claude-pr 17801 --exited    include exited sessions too
  claude-pr -c 17801          only sessions that created it
  claude-pr                   all live sessions and the PRs they track
  claude-pr -o                only sessions with an open PR (draft or not)

Sessions are read read-only from $CLAUDE_CONFIG_DIR (falling back to
~/.claude-personal, ~/.claude, ~/.config/claude). Liveness uses /proc (Linux);
--status needs the gh CLI authenticated.
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
			pos = append(pos, a)
		}
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
	default: // auto: on under WezTerm on a TTY (the open-uri handler turns the link into a resume)
		wez := os.Getenv("WEZTERM_PANE") != "" || os.Getenv("TERM_PROGRAM") == "WezTerm"
		resumeLinks = tty && wez && !forceURL
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
	runListMode(creatorOnly, showStatus, showEmpty, includeExited, openOnly, filter, roots)
}
