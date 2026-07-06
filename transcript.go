package main

import (
	"bytes"
	"encoding/json"
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
