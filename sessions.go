package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type regSession struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Cwd       string `json:"cwd"`
	Pid       int    `json:"pid"`
	Alive     bool   `json:"-"`
	CfgDir    string `json:"-"` // config dir whose registry listed the session
}

// readSessions returns the running sessions from each config's sessions/
// registry (the daemon prunes the registry on exit, so this is live-only),
// de-duplicated by session ID.
func readSessions(roots []string) []regSession {
	bySID := map[string]regSession{}
	seenDir := map[string]bool{}
	for _, root := range roots {
		cfgDir := filepath.Dir(root)
		dir := filepath.Join(cfgDir, "sessions")
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
			s.CfgDir = cfgDir
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
