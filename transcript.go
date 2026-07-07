package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type record struct {
	Type         string `json:"type"`
	CustomTitle  string `json:"customTitle"`
	Timestamp    string `json:"timestamp"`
	PrNumber     int    `json:"prNumber"`
	PrRepository string `json:"prRepository"`
	PrURL        string `json:"prUrl"` // full web URL; GitLab MRs carry a /-/merge_requests/ URL here
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

// cfgFromPath returns the Claude config dir for a transcript path (the part
// before "/projects/"), which `claude --resume` needs as CLAUDE_CONFIG_DIR.
func cfgFromPath(transcriptPath string) string {
	if i := strings.Index(transcriptPath, "/projects/"); i >= 0 {
		return transcriptPath[:i]
	}
	return ""
}

// candidateProjectDirs lists the projects dirs to scan. CLAUDE_CONFIG_DIR, when
// set, is authoritative — only its projects dir is scanned, matching how Claude
// Code itself treats the var — so pointing it at a separate config (e.g. one for
// an internal GitLab instance) does not bleed sessions from the default location
// into the listing. When it is unset, Claude Code's documented default ~/.claude
// applies.
func candidateProjectDirs(cfgDir, home string) []string {
	if cfgDir != "" {
		return []string{filepath.Join(cfgDir, "projects")}
	}
	if home == "" {
		return nil
	}
	return []string{filepath.Join(home, ".claude", "projects")}
}

func discoverRoots() []string {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var roots []string
	for _, c := range candidateProjectDirs(os.Getenv("CLAUDE_CONFIG_DIR"), home) {
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
	url     string // the web URL Claude Code recorded (empty for legacy/created-only refs)
}

// webURL is the PR/MR web address: the URL Claude Code recorded (which for a
// GitLab MR is the correct /-/merge_requests/ link on its host), falling back to
// a github.com/<repo>/pull/<n> reconstruction only when none was stored.
func (p prRef) webURL() string {
	if p.url != "" {
		return p.url
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", p.repo, p.num)
}

// scanTracked returns the PRs a session references via "pr-link" records (its
// tracked set), unioned with PRs it created (a bare PR-URL line emitted by one
// of its `gh pr create` calls), with created ones flagged.
func scanTracked(data []byte) []prRef {
	cmdByID := map[string]string{}
	type res struct{ tuid, txt string }
	var results []res
	trackedURL := map[string]string{} // key -> prUrl from the pr-link record
	createdURL := map[string]string{} // key -> URL from a `gh pr create` result

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
			trackedURL[rec.PrRepository+"#"+strconv.Itoa(rec.PrNumber)] = rec.PrURL
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
			createdURL[m[1]+"#"+m[2]] = strings.TrimSpace(m[0])
		}
	}

	keys := map[string]bool{}
	for k := range trackedURL {
		keys[k] = true
	}
	for k := range createdURL {
		keys[k] = true
	}
	refs := make([]prRef, 0, len(keys))
	for k := range keys {
		i := strings.LastIndex(k, "#")
		num, _ := strconv.Atoi(k[i+1:])
		url := trackedURL[k]
		if url == "" {
			url = createdURL[k]
		}
		_, isCreated := createdURL[k]
		refs = append(refs, prRef{repo: k[:i], num: num, created: isCreated, url: url})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].repo != refs[j].repo {
			return refs[i].repo < refs[j].repo
		}
		return refs[i].num < refs[j].num
	})
	return refs
}
