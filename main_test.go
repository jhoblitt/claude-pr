package main

import (
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
		"12a":                                     {"", 0, false},
		"not-a-pr":                                {"", 0, false},
		"https://gitlab.com/o/r/pull/5":           {"", 0, false},
	}
	for in, w := range cases {
		repo, num, ok := parsePRArg(in)
		if repo != w.repo || num != w.num || ok != w.ok {
			t.Errorf("parsePRArg(%q) = (%q,%d,%v), want (%q,%d,%v)", in, repo, num, ok, w.repo, w.num, w.ok)
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
