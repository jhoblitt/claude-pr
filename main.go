// claude-pr reverse-maps a GitHub PR number to the Claude Code session(s)
// responsible for it, by scanning local session transcripts (JSONL).
//
// A PR is created by `gh pr create`, whose stdout includes the new PR URL on its
// own line, recorded as a Bash tool_result in the transcript. The CREATOR is the
// session that actually *invokes* `gh pr create` (the command, not the string in
// some echo/comment) and whose result carries that URL line; sessions that only
// edited/readied/viewed/mentioned the PR are "touched". Earliest timestamp wins.
//
// Session identity is shown as the /rename title when one exists (from
// "custom-title" records), with the UUID in brackets, else just the UUID.
//
// With no PR number, it lists the currently-live sessions (from the daemon's
// sessions/ registry, process still running) as aligned columns —
// name · uuid · cwd · status — each followed by a tree of the PRs it is tracking
// (from "pr-link" records), with created PRs flagged. Each PR id is a clickable
// terminal hyperlink (OSC 8) to the PR. Output is colored when stdout is a TTY
// (honoring NO_COLOR); unnamed sessions show "(unnamed)".
//
// Usage: claude-pr [flags] [<PR_NUMBER> [owner/repo]]
//        -c / --creator   PR mode: print only the true creator.
//                         list mode: show only PRs the session created.
//        -a / --all       list mode: also list sessions with no tracked PRs
//                         (hidden by default).
//        --exited         list mode: also include exited (no longer running)
//                         sessions, shown with an "exited" status.
//        --status         list mode: annotate each PR with live GitHub state
//                         (OPEN/MERGED/CLOSED, draft, checks, review) via `gh`.
//        --url            print raw PR URLs instead of terminal hyperlinks.
//        --full-uuid      show the full session UUID (default: 8-char prefix).
//        --color/--no-color  force or disable ANSI color (default: auto).
//        --resume-links/--no-resume-links  make each session name/uuid a
//                         clickable resume link (auto-on under WezTerm on a TTY;
//                         needs an open-uri handler in the wezterm config).
//        -h / --help      show usage and exit.
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

// resumeURI is the custom-scheme target a WezTerm open-uri handler turns into
// `claude --resume <id>` in the session's cwd. A path-based form (no query
// string) is used because WezTerm only treats the result as a clickable
// hyperlink without a "?...&..." query: claude-resume://r/<id><abs-cwd>.
func resumeURI(uuid, cwdAbs string) string {
	return "claude-resume://r/" + uuid + cwdAbs
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

// sessionLoc describes where a transcript sits: the owning session UUID, the
// project, an optional subagent suffix, and the transcript to read the /rename
// title from (the parent session for subagent transcripts).
type sessionLoc struct {
	uuid, proj, suffix, titleFile string
}

func resolveLoc(path string) sessionLoc {
	if i := strings.Index(path, "/subagents/"); i >= 0 {
		sessDir := path[:i]
		agent := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		return sessionLoc{
			uuid:      filepath.Base(sessDir),
			proj:      filepath.Base(filepath.Dir(sessDir)),
			suffix:    " (subagent " + agent + ")",
			titleFile: sessDir + ".jsonl",
		}
	}
	return sessionLoc{
		uuid:      strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		proj:      filepath.Base(filepath.Dir(path)),
		titleFile: path,
	}
}

type result struct {
	line    string
	created bool
}

// scanFile returns the output line for one transcript, whether the session truly
// created the PR, and ok=false if the PR's URL never appears as a bare-line
// tool_result there.
func scanFile(data []byte, reLine *regexp.Regexp, path string) (result, bool) {
	cmdByID := map[string]string{}
	type match struct{ ts, toolUseID string }
	var matches []match

	for _, raw := range bytes.Split(data, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var rec record
		if json.Unmarshal(raw, &rec) != nil || rec.Message == nil {
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
				if reLine.MatchString(resultText(it.Content)) {
					ts := rec.Timestamp
					if ts == "" {
						ts = "?"
					}
					matches = append(matches, match{ts, it.ToolUseID})
				}
			}
		}
	}
	if len(matches) == 0 {
		return result{}, false
	}
	minTS := matches[0].ts
	created := false
	for _, m := range matches {
		if m.ts < minTS {
			minTS = m.ts
		}
		if reCreate.MatchString(cmdByID[m.toolUseID]) {
			created = true
		}
	}

	loc := resolveLoc(path)
	title := latestCustomTitle(data)
	if loc.titleFile != path {
		if pdata, err := os.ReadFile(loc.titleFile); err == nil {
			title = latestCustomTitle(pdata)
		}
	}
	sid := loc.uuid
	if title != "" {
		sid = title + " [" + loc.uuid + "]"
	}
	sid += loc.suffix

	status := "touched "
	if created {
		status = "CREATOR "
	}
	return result{line: status + minTS + "  " + sid + "  (" + loc.proj + ")", created: created}, true
}

func runPRMode(pos []string, creatorOnly bool, roots []string) {
	pr := pos[0]
	slug := `[^/\s]+/[^/\s]+`
	if len(pos) > 1 && pos[1] != "" {
		slug = regexp.QuoteMeta(pos[1])
	}
	reLine := regexp.MustCompile(`(?m)^https?://github\.com/` + slug + `/pull/` + regexp.QuoteMeta(pr) + `[ \t]*\r?$`)
	prefilter := []byte("/pull/" + pr)

	var lines []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil || !bytes.Contains(data, prefilter) {
				return nil
			}
			if res, ok := scanFile(data, reLine, p); ok && (!creatorOnly || res.created) {
				lines = append(lines, res.line)
			}
			return nil
		})
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
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

func runListMode(creatorOnly, showStatus, showEmpty, includeExited bool, roots []string) {
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
		name, uuid, cwdAbs, status string
		prs                        []prRef
	}
	var rows []row
	for _, s := range sessions {
		var prs []prRef
		name, cwd := s.Name, s.Cwd
		if tf := idx[s.SessionID]; tf != "" {
			if data, err := os.ReadFile(tf); err == nil {
				prs = scanTracked(data)
				if !s.Alive { // exited: registry is gone, so pull metadata from the transcript
					name, cwd = latestCustomTitle(data), cwdFromTranscript(data)
				}
			}
		}
		if creatorOnly {
			var only []prRef
			for _, p := range prs {
				if p.created {
					only = append(only, p)
				}
			}
			prs = only
		}
		if len(prs) == 0 && !showEmpty {
			continue
		}
		st := s.Status
		if !s.Alive {
			st = "exited"
		} else if st == "" {
			st = "?"
		}
		rows = append(rows, row{name, s.SessionID, cwd, st, prs})
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

	statusByKey := map[string]string{}
	if showStatus {
		if _, err := exec.LookPath("gh"); err != nil {
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
			uri := resumeURI(r.uuid, r.cwdAbs)
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
  wezterm.on('open-uri', function(window, pane, uri)
    -- claude-resume://r/<id><abs-cwd>, e.g. claude-resume://r/<uuid>/home/me/proj
    local id, cwd = uri:match('^claude%-resume://r/([^/]+)(/?.*)$')
    if id then
      wezterm.log_info('claude-pr: resume ' .. uri) -- visible in the debug overlay (Ctrl+Shift+L)
      if cwd == '' then cwd = nil end
      window:perform_action(act.SpawnCommandInNewTab {
        cwd = cwd,
        -- login shell so CLAUDE_CONFIG_DIR is set; cwd makes --resume's scope match
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
  claude-pr [flags] [<PR_NUMBER> [owner/repo]]

Modes:
  with a PR number   show which session(s) created or merely touched that PR.
  no arguments       list currently-live sessions and the PRs each is tracking.

Flags:
  -c, --creator    PR mode: print only the true creator.
                   list mode: show only PRs the session created.
  -a, --all        list mode: also show sessions with no tracked PRs.
      --exited     list mode: also include exited (no longer running) sessions,
                   shown with an "exited" status.
      --status     list mode: annotate each PR with live GitHub state
                   (OPEN/MERGED/CLOSED, draft, checks, review) via gh.
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
  claude-pr 17801           which session created PR #17801
  claude-pr -c 17801        just the creator
  claude-pr                 live sessions and the PRs they track
  claude-pr --all --status  include empty sessions, with live PR state

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

	if len(pos) == 0 {
		runListMode(creatorOnly, showStatus, showEmpty, includeExited, roots)
		return
	}
	if showStatus {
		fmt.Fprintln(os.Stderr, "claude-pr: --status applies to list mode (no PR argument); ignoring")
	}
	runPRMode(pos, creatorOnly, roots)
}
