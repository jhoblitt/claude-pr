package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// reMRURL parses a GitLab merge request URL:
//
//	scheme://<host>/<project path>/-/merge_requests/<iid>
//
// The project path may contain nested group segments, so it is captured
// non-greedily up to the "/-/merge_requests/" delimiter.
var reMRURL = regexp.MustCompile(`^https?://([^/\s]+)/(\S+?)/-/merge_requests/(\d+)(?:[/?#].*)?$`)

// parseMRURL extracts (host, projectPath, iid) from a GitLab MR URL.
func parseMRURL(u string) (host, project string, iid int, ok bool) {
	m := reMRURL.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return "", "", 0, false
	}
	return m[1], m[2], n, true
}

// isGitLab reports whether a ref is a GitLab merge request, decided from the
// recorded URL (GitLab MR URLs carry the "/-/merge_requests/" path).
func (p prRef) isGitLab() bool {
	return strings.Contains(p.url, "/-/merge_requests/")
}

// sigil is the provider's reference marker: "!" for a GitLab MR, "#" otherwise.
func (p prRef) sigil() string {
	if p.isGitLab() {
		return "!"
	}
	return "#"
}

// displayRef is the fully-qualified human reference, e.g. "rook/rook#17801" or
// "group/sub/project!437".
func (p prRef) displayRef() string {
	return p.repo + p.sigil() + strconv.Itoa(p.num)
}

// fetchMRStatus returns a compact live status for a GitLab MR via `glab`, in the
// same shape as the GitHub path: "OPEN draft ✓", "MERGED", "CLOSED ✗", or
// "status? <reason>".
func fetchMRStatus(p prRef) string {
	host, project, iid, ok := parseMRURL(p.url)
	if !ok {
		return "status? unparseable MR url"
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "glab", "mr", "view", strconv.Itoa(iid), "-R", project, "-F", "json")
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), "GITLAB_HOST="+host) // select the self-hosted instance + its token
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "status? glab timed out"
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
	return formatGitLabMR(out.Bytes())
}

// formatGitLabMR renders `glab mr view -F json` output into the compact status
// string. It is split out from the exec so it can be unit-tested with fixtures.
func formatGitLabMR(jsonOut []byte) string {
	var v struct {
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		WorkInProgress bool   `json:"work_in_progress"`
		Pipeline       *struct {
			Status string `json:"status"`
		} `json:"pipeline"`
		HeadPipeline *struct {
			Status string `json:"status"`
		} `json:"head_pipeline"`
	}
	if json.Unmarshal(jsonOut, &v) != nil {
		return "status? parse error"
	}
	state := strings.ToUpper(v.State)
	switch v.State {
	case "opened":
		state = "OPEN"
	case "merged":
		state = "MERGED"
	case "closed":
		state = "CLOSED"
	case "locked":
		state = "LOCKED"
	}
	parts := []string{state}
	if state == "OPEN" && (v.Draft || v.WorkInProgress) {
		parts = append(parts, "draft")
	}
	pstatus := ""
	if v.HeadPipeline != nil {
		pstatus = v.HeadPipeline.Status
	} else if v.Pipeline != nil {
		pstatus = v.Pipeline.Status
	}
	switch pstatus {
	case "success":
		parts = append(parts, "✓")
	case "failed":
		parts = append(parts, "✗")
	case "running", "pending", "created", "preparing", "waiting_for_resource", "scheduled":
		parts = append(parts, "⧖")
	}
	return strings.Join(parts, " ")
}
