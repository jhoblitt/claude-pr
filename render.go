package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

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

// visWidth is a cell's visible width in columns: rune count, not byte count,
// since names and paths need not be ASCII. (East-Asian double-width glyphs
// would need a width table; not worth a dependency here.)
func visWidth(s string) int {
	return utf8.RuneCountInString(s)
}

// field left-pads s to visible width w (measured plain), then styles the cell.
func field(s string, w int, style string) string {
	if pad := w - visWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return col(style, s)
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

const unnamed = "(unnamed)"

// row is one session line in the listing.
type row struct {
	name, uuid, cwdAbs, cfg, status string
	prs                             []prRef
}

func (r row) displayName() string {
	if r.name != "" {
		return r.name
	}
	return unnamed
}

// rowLess orders sessions: most attention-seeking status first, then named
// before unnamed, then by name, then by uuid.
func rowLess(a, b row) bool {
	if ra, rb := statusRank(a.status), statusRank(b.status); ra != rb {
		return ra < rb
	}
	if ua, ub := a.name == "", b.name == ""; ua != ub {
		return !ua
	}
	if na, nb := a.displayName(), b.displayName(); na != nb {
		return na < nb
	}
	return a.uuid < b.uuid
}

// pruneOpen keeps only OPEN PRs in each row and drops rows left with none. It
// also returns, sorted, "key: status" strings for PRs whose live state could
// not be determined — those are excluded too, but the caller should say so
// rather than let them vanish silently.
func pruneOpen(rows []row, statusByKey map[string]string) ([]row, []string) {
	unknown := map[string]bool{}
	var kept []row
	for _, r := range rows {
		var open []prRef
		for _, p := range r.prs {
			st := statusByKey[prKey(p)]
			switch {
			case isOpenStatus(st):
				open = append(open, p)
			case st == "" || strings.HasPrefix(st, "status?"):
				if st == "" {
					st = "status unknown"
				}
				unknown[prKey(p)+": "+st] = true
			}
		}
		if len(open) > 0 {
			r.prs = open
			kept = append(kept, r)
		}
	}
	msgs := make([]string, 0, len(unknown))
	for m := range unknown {
		msgs = append(msgs, m)
	}
	sort.Strings(msgs)
	return kept, msgs
}

// runListMode renders the session listing (optionally filtered to one PR) and
// returns the process exit code: 0 on a match/normal listing, 1 when a lookup
// or --open filter matched nothing.
func runListMode(creatorOnly, showStatus, showEmpty, includeExited, openOnly bool, filter *prQuery, roots []string) int {
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
				} else {
					if name == "" { // registry entries can lack the /rename title
						name = latestCustomTitle(data)
					}
					if cwd == "" {
						cwd = cwdFromTranscript(data)
					}
				}
			}
		}
		cfg := cfgFromPath(tf)
		if cfg == "" {
			cfg = s.CfgDir // no transcript found; the registry knows the config dir
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

	sort.Slice(rows, func(i, j int) bool { return rowLess(rows[i], rows[j]) })

	if filter != nil && len(rows) == 0 {
		hint := ""
		if !includeExited {
			hint = " (use --exited to include exited sessions)"
		}
		fmt.Fprintln(os.Stderr, "claude-pr: no session references that PR"+hint)
		return 1
	}

	statusByKey := map[string]string{}
	if showStatus {
		if _, err := exec.LookPath("gh"); err != nil {
			if openOnly {
				fmt.Fprintln(os.Stderr, "claude-pr: --open needs the 'gh' CLI on PATH to determine PR state")
				return 1
			}
			fmt.Fprintln(os.Stderr, "claude-pr: --status needs the 'gh' CLI on PATH; skipping status")
		} else {
			seen := map[string]bool{}
			var todo []prRef
			for _, r := range rows {
				for _, p := range r.prs {
					if k := prKey(p); !seen[k] {
						seen[k] = true
						todo = append(todo, p)
					}
				}
			}
			statusByKey = fetchStatuses(todo)
		}
	}

	if openOnly {
		var unknown []string
		rows, unknown = pruneOpen(rows, statusByKey)
		for _, u := range unknown {
			fmt.Fprintln(os.Stderr, "claude-pr: --open: excluding "+u)
		}
		if len(rows) == 0 {
			if filter != nil {
				fmt.Fprintln(os.Stderr, "claude-pr: that PR is not open")
			} else {
				hint := ""
				if !includeExited {
					hint = " (use --exited to include exited sessions)"
				}
				fmt.Fprintln(os.Stderr, "claude-pr: no session has an open tracked PR"+hint)
			}
			return 1
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
		return prKey(p)
	}
	prURL := func(p prRef) string {
		return fmt.Sprintf("https://github.com/%s/pull/%d", p.repo, p.num)
	}

	wName, wUUID, wCwd, wRef := visWidth(unnamed), 0, 0, 0
	for _, r := range rows {
		if n := visWidth(r.displayName()); n > wName {
			wName = n
		}
		if n := visWidth(uuidDisp(r.uuid)); n > wUUID {
			wUUID = n
		}
		if n := visWidth(abbrevHome(r.cwdAbs)); n > wCwd {
			wCwd = n
		}
		for _, p := range r.prs {
			if n := visWidth(refText(p)); n > wRef {
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
		nameText, uuidText := r.displayName(), uuidDisp(r.uuid)
		nameCell, uuidCell := col(nameStyle, nameText), col(cDim, uuidText)
		if resumeLinks {
			uri := resumeURI(r.uuid, r.cfg, r.cwdAbs)
			nameCell, uuidCell = osc8(nameCell, uri), osc8(uuidCell, uri)
		}
		nameCell += strings.Repeat(" ", wName-visWidth(nameText))
		uuidCell += strings.Repeat(" ", wUUID-visWidth(uuidText))
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
			if pad := wRef - visWidth(rt); pad > 0 {
				ref += strings.Repeat(" ", pad)
			}
			line := "  " + col(cDim, conn) + " " + ref
			if showRawURL {
				line += "  " + col(cDim, prURL(p))
			}
			if st := statusByKey[prKey(p)]; st != "" {
				line += "  [" + colorizePRStatus(st) + "]"
			}
			if p.created {
				line += "  " + col(cGreen, "(created)")
			}
			fmt.Println(line)
		}
	}
	return 0
}
