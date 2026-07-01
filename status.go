package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

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
