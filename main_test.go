package main

import (
	"os"
	"strings"
	"testing"
)

func TestParsePRArg(t *testing.T) {
	type want struct {
		repo string
		num  int
		ok   bool
	}
	cases := map[string]want{
		"1234":      {"", 1234, true},
		"#1234":     {"", 1234, true},
		"  #1234  ": {"", 1234, true},
		"https://github.com/rook/rook/pull/17801": {"rook/rook", 17801, true},
		"http://github.com/o/r/pull/5":            {"o/r", 5, true},
		"https://github.com/o/r/pull/5/files":     {"o/r", 5, true},
		"https://github.com/o/r/pull/5?x=1":       {"o/r", 5, true},
		"":                                        {"", 0, false},
		"#":                                       {"", 0, false},
		"0":                                       {"", 0, false},
		"#0":                                      {"", 0, false},
		"12a":                                     {"", 0, false},
		"not-a-pr":                                {"", 0, false},
		"https://gitlab.com/o/r/pull/5":           {"", 0, false},
		"https://github.com/o/r/pull/0":           {"", 0, false},
	}
	for in, w := range cases {
		repo, num, ok := parsePRArg(in)
		if repo != w.repo || num != w.num || ok != w.ok {
			t.Errorf("parsePRArg(%q) = (%q,%d,%v), want (%q,%d,%v)", in, repo, num, ok, w.repo, w.num, w.ok)
		}
	}
}

func TestCandidateProjectDirs(t *testing.T) {
	// CLAUDE_CONFIG_DIR set => authoritative, only its projects dir.
	got := candidateProjectDirs("/cfg/enterprise", "/home/me")
	if len(got) != 1 || got[0] != "/cfg/enterprise/projects" {
		t.Errorf("cfg set: got %v, want [/cfg/enterprise/projects]", got)
	}
	// Unset => Claude Code's documented default, ~/.claude only.
	got = candidateProjectDirs("", "/home/me")
	if len(got) != 1 || got[0] != "/home/me/.claude/projects" {
		t.Errorf("cfg unset: got %v, want [/home/me/.claude/projects]", got)
	}
	// Neither set => nothing (no home => no default).
	if got := candidateProjectDirs("", ""); got != nil {
		t.Errorf("no cfg, no home: got %v, want nil", got)
	}
}

func TestScanTrackedPreservesURL(t *testing.T) {
	mrURL := "https://gitlab.example.com/group/subgroup/project/-/merge_requests/437"
	data := []byte(`{"type":"pr-link","prNumber":437,"prRepository":"group/subgroup/project","prUrl":"` + mrURL + `"}`)
	refs := scanTracked(data)
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	r := refs[0]
	if r.repo != "group/subgroup/project" || r.num != 437 {
		t.Errorf("repo/num = %q/%d, want group/subgroup/project/437", r.repo, r.num)
	}
	if r.url != mrURL {
		t.Errorf("url = %q, want %q", r.url, mrURL)
	}
	if got := r.webURL(); got != mrURL {
		t.Errorf("webURL() = %q, want the stored MR URL", got)
	}
}

func TestPRWebURL(t *testing.T) {
	// A stored URL (GitLab MR) is used verbatim.
	mr := prRef{repo: "grp/sub/proj", num: 5, url: "https://gl.example.com/grp/sub/proj/-/merge_requests/5"}
	if got := mr.webURL(); got != mr.url {
		t.Errorf("webURL() = %q, want stored %q", got, mr.url)
	}
	// No stored URL => github.com fallback reconstruction.
	legacy := prRef{repo: "o/r", num: 9}
	if got, want := legacy.webURL(), "https://github.com/o/r/pull/9"; got != want {
		t.Errorf("webURL() fallback = %q, want %q", got, want)
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(self) = false, want true")
	}
	for _, pid := range []int{0, -1} {
		if pidAlive(pid) {
			t.Errorf("pidAlive(%d) = true, want false", pid)
		}
	}
}

func TestFieldUnicodePadding(t *testing.T) {
	colorEnabled = false
	if got := field("café", 6, ""); got != "café  " {
		t.Errorf("field(café, 6) = %q, want %q", got, "café  ")
	}
	if got := visWidth("café"); got != 4 {
		t.Errorf("visWidth(café) = %d, want 4", got)
	}
}

func TestRowLess(t *testing.T) {
	busy := row{name: "b", uuid: "1", status: "busy"}
	idle := row{name: "a", uuid: "2", status: "idle"}
	unnamedIdle := row{uuid: "3", status: "idle"}
	idleB := row{name: "b", uuid: "4", status: "idle"}
	cases := []struct {
		a, b row
		want bool
		why  string
	}{
		{busy, idle, true, "busy before idle regardless of name"},
		{idle, unnamedIdle, true, "named before unnamed at equal rank"},
		{idle, idleB, true, "same rank sorts by name"},
		{idleB, idle, false, "name order is asymmetric"},
	}
	for _, c := range cases {
		if got := rowLess(c.a, c.b); got != c.want {
			t.Errorf("rowLess: %s: got %v, want %v", c.why, got, c.want)
		}
	}
}

func TestPruneOpen(t *testing.T) {
	open := prRef{repo: "o/r", num: 1}
	merged := prRef{repo: "o/r", num: 2}
	failed := prRef{repo: "o/r", num: 3}
	rows := []row{
		{name: "mixed", prs: []prRef{open, merged, failed}},
		{name: "allmerged", prs: []prRef{merged}},
	}
	status := map[string]string{
		"o/r#1": "OPEN draft ✓5",
		"o/r#2": "MERGED ✓6",
		"o/r#3": "status? HTTP 401",
	}
	kept, unknown := pruneOpen(rows, status)
	if len(kept) != 1 || kept[0].name != "mixed" {
		t.Fatalf("pruneOpen kept = %+v, want only the mixed row", kept)
	}
	if len(kept[0].prs) != 1 || kept[0].prs[0].num != 1 {
		t.Errorf("kept prs = %+v, want only o/r#1", kept[0].prs)
	}
	if len(unknown) != 1 || unknown[0] != "o/r#3: status? HTTP 401" {
		t.Errorf("unknown = %q, want the o/r#3 fetch failure", unknown)
	}
}

func TestIsOpenStatus(t *testing.T) {
	open := []string{"OPEN", "OPEN draft", "OPEN ✓5", "OPEN draft ✓57 ✗1 REVIEW_REQUIRED"}
	notOpen := []string{"", "MERGED", "MERGED ✓58", "CLOSED", "status? not found", "OPENING"}
	for _, s := range open {
		if !isOpenStatus(s) {
			t.Errorf("isOpenStatus(%q) = false, want true", s)
		}
	}
	for _, s := range notOpen {
		if isOpenStatus(s) {
			t.Errorf("isOpenStatus(%q) = true, want false", s)
		}
	}
}

func TestPctEncode(t *testing.T) {
	cases := map[string]string{
		"/home/jhoblitt/.claude-personal": "%2Fhome%2Fjhoblitt%2F.claude-personal",
		"abc":                             "abc",
		"a b":                             "a%20b",
		"a&b":                             "a%26b",
		"-_.~":                            "-_.~",
	}
	for in, want := range cases {
		if got := pctEncode(in); got != want {
			t.Errorf("pctEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCfgFromPath(t *testing.T) {
	cases := map[string]string{
		"/home/me/.claude-personal/projects/-proj/uuid.jsonl": "/home/me/.claude-personal",
		"/home/me/.claude/projects/-p/x.jsonl":                "/home/me/.claude",
		"no-projects-marker":                                  "",
	}
	for in, want := range cases {
		if got := cfgFromPath(in); got != want {
			t.Errorf("cfgFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUUIDDisp(t *testing.T) {
	full := "8ffe82dd-303a-49c4-93a0-2b3b32624006"
	fullUUID = false
	if got := uuidDisp(full); got != "8ffe82dd" {
		t.Errorf("short uuidDisp = %q, want 8ffe82dd", got)
	}
	if got := uuidDisp("abc"); got != "abc" {
		t.Errorf("uuidDisp(short) = %q, want abc", got)
	}
	fullUUID = true
	if got := uuidDisp(full); got != full {
		t.Errorf("full uuidDisp = %q, want %q", got, full)
	}
	fullUUID = false
}

func TestResumeURI(t *testing.T) {
	got := resumeURI("uuid-1", "/cfg", "/home/me/p")
	want := "claude-resume://r/uuid-1/%2Fcfg/%2Fhome%2Fme%2Fp"
	if got != want {
		t.Errorf("resumeURI = %q, want %q", got, want)
	}
}

func TestStatusRankOrdering(t *testing.T) {
	// strictly increasing => waiting first, exited last
	order := []string{"waiting", "busy", "idle", "shell", "unknown", "exited"}
	for i := 1; i < len(order); i++ {
		if statusRank(order[i-1]) >= statusRank(order[i]) {
			t.Errorf("rank(%q)=%d should be < rank(%q)=%d",
				order[i-1], statusRank(order[i-1]), order[i], statusRank(order[i]))
		}
	}
}

func TestCwdFromTranscript(t *testing.T) {
	data := []byte(`{"type":"summary"}
{"type":"user","cwd":"/home/me/proj","message":{}}
`)
	if got := cwdFromTranscript(data); got != "/home/me/proj" {
		t.Errorf("cwdFromTranscript = %q, want /home/me/proj", got)
	}
	if got := cwdFromTranscript([]byte(`{"type":"x"}`)); got != "" {
		t.Errorf("cwdFromTranscript(none) = %q, want empty", got)
	}
}

func TestLatestCustomTitle(t *testing.T) {
	data := []byte(`{"type":"custom-title","customTitle":"first"}
{"type":"other"}
{"type":"custom-title","customTitle":"second"}
`)
	if got := latestCustomTitle(data); got != "second" {
		t.Errorf("latestCustomTitle = %q, want second", got)
	}
	if got := latestCustomTitle([]byte(`{"type":"x"}`)); got != "" {
		t.Errorf("latestCustomTitle(none) = %q, want empty", got)
	}
}

func TestReCreateCommandPosition(t *testing.T) {
	match := []string{
		"gh pr create --repo x",
		"cd /tmp\ngh pr create --draft",
		"foo && gh pr create",
		"BODY=$(mktemp); gh pr create",
	}
	noMatch := []string{
		"echo gh pr create",
		`echo "calls gh pr create here"`,
		"gh pr edit 1",
		"gh pr created",
	}
	for _, s := range match {
		if !reCreate.MatchString(s) {
			t.Errorf("reCreate should match %q", s)
		}
	}
	for _, s := range noMatch {
		if reCreate.MatchString(s) {
			t.Errorf("reCreate should NOT match %q", s)
		}
	}
}

func TestInjectBlockIdempotent(t *testing.T) {
	block := weztermBlock()
	base := "local config = {}\nreturn config\n"

	out1, act1 := injectBlock(base, block)
	if act1 != "added" {
		t.Errorf("first inject action = %q, want added", act1)
	}
	if !strings.Contains(out1, block) {
		t.Fatal("inject did not include the handler block")
	}
	if strings.Index(out1, wezBegin) > strings.Index(out1, "return config") {
		t.Error("handler block must be inserted before the top-level return")
	}

	out2, _ := injectBlock(out1, block)
	if out2 != out1 {
		t.Error("re-injecting the same block is not idempotent")
	}
}

func TestInjectBlockReturnMatching(t *testing.T) {
	block := weztermBlock()

	// "returnvalue = 1" is an assignment, not a return: with no real top-level
	// return the block must be appended, not inserted before the assignment.
	out, act := injectBlock("returnvalue = 1\n", block)
	if act != "added" {
		t.Fatalf("action = %q, want added", act)
	}
	if strings.Index(out, block) < strings.Index(out, "returnvalue = 1") {
		t.Error("block must not be inserted before a non-return line")
	}

	// return{...} is a real return and must stay last.
	out, _ = injectBlock("local config = {}\nreturn{}\n", block)
	if strings.Index(out, block) > strings.Index(out, "return{}") {
		t.Error("block must be inserted before a top-level return{}")
	}
}
