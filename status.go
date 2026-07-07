package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// statusTimeout bounds each `gh pr view` call so one hung network request
// cannot wedge the whole listing.
const statusTimeout = 30 * time.Second

// fetchPRStatus returns a compact live status for a PR via `gh`, e.g.
// "OPEN draft ✓57 ✗1 REVIEW_REQUIRED" or "MERGED ✓58", or "status? <reason>".
func fetchPRStatus(repo string, num int) string {
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(num), "-R", repo,
		"--json", "state,isDraft,reviewDecision,statusCheckRollup")
	// Killing gh at the deadline is not enough: a grandchild holding the
	// stdout/stderr pipes would keep Wait blocked. WaitDelay force-closes them.
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "status? gh timed out"
		}
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

// prKey is the canonical "repo#N" key for status maps and dedup. It is an
// internal key (always "#", never the "!" MR sigil); use displayRef for output.
func prKey(p prRef) string {
	return fmt.Sprintf("%s#%d", p.repo, p.num)
}

// cliOnPath reports whether a command is resolvable on PATH.
func cliOnPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// fetchStatus resolves one ref's live status via the provider's CLI.
func fetchStatus(p prRef) string {
	if p.isGitLab() {
		return fetchMRStatus(p)
	}
	return fetchPRStatus(p.repo, p.num)
}

// fetchStatuses resolves status for each distinct PR/MR concurrently (bounded).
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
			s := fetchStatus(p)
			mu.Lock()
			out[prKey(p)] = s
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
