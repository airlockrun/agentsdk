package agentsdk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dop251/goja"
)

// editContentType is the MIME type stamped on editor output (always text).
const editContentType = "text/plain; charset=utf-8"

// applyEdit streams src through fn and lands the edited text. Unlike codec
// transforms, an edit MAY target its own source (in-place, sed -i): openCached
// fully materializes src before any write, so writing the src key can't race
// the src read.
func (r *run) applyEdit(ctx context.Context, src, dst string, fn streamFunc) (*transformResult, error) {
	srcCanon, err := normalizePath(src)
	if err != nil {
		return nil, err
	}
	dstCanon := ""
	if dst != "" {
		if dstCanon, err = normalizePath(dst); err != nil {
			return nil, err
		}
	}
	return r.streamThrough(ctx, srcCanon, dstCanon, editContentType, ".txt", true, fn)
}

// --- fileEditLines: structured, line-addressed edits ---

// lineEdit is one structured edit over original 1-based line numbers.
type lineEdit struct {
	from     int    // 1-based first line (ignored for append)
	count    int    // lines covered; 0 = insert before `from`
	text     string // replacement / inserted / appended text (verbatim)
	hasText  bool
	isAppend bool // append at EOF
}

func (r *run) editLines(ctx context.Context, src, dst string, edits []lineEdit) (*transformResult, error) {
	if err := validateLineEdits(edits); err != nil {
		return nil, err
	}
	return r.applyEdit(ctx, src, dst, editLinesFunc(edits))
}

// editLinesFunc builds a streamFunc applying the (validated, sorted) edits in
// one pass over original line numbers.
func editLinesFunc(edits []lineEdit) streamFunc {
	var ranges []lineEdit
	var appends []lineEdit
	for _, e := range edits {
		if e.isAppend {
			appends = append(appends, e)
		} else {
			ranges = append(ranges, e)
		}
	}
	return func(dst io.Writer, src io.Reader) error {
		w := bufio.NewWriter(dst)
		sc := newFileScanner(src)
		ei, lineNo := 0, 0
		for sc.Scan() {
			lineNo++
			// Pure inserts (count 0) positioned before this line.
			for ei < len(ranges) && ranges[ei].count == 0 && ranges[ei].from == lineNo {
				if _, err := io.WriteString(w, ranges[ei].text); err != nil {
					return err
				}
				ei++
			}
			// A replace/delete range covering this line.
			if ei < len(ranges) && ranges[ei].count > 0 &&
				lineNo >= ranges[ei].from && lineNo < ranges[ei].from+ranges[ei].count {
				if lineNo == ranges[ei].from && ranges[ei].hasText {
					if _, err := io.WriteString(w, ranges[ei].text); err != nil {
						return err
					}
				}
				if lineNo == ranges[ei].from+ranges[ei].count-1 {
					ei++
				}
				continue // original line deleted
			}
			if _, err := io.WriteString(w, sc.Text()); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		if err := sc.Err(); err != nil {
			return scanErr(err)
		}
		// Inserts positioned at/after EOF, then appends.
		for ; ei < len(ranges); ei++ {
			if ranges[ei].count == 0 {
				if _, err := io.WriteString(w, ranges[ei].text); err != nil {
					return err
				}
			}
		}
		for _, a := range appends {
			if _, err := io.WriteString(w, a.text); err != nil {
				return err
			}
		}
		return w.Flush()
	}
}

// parseLineEdits reads the JS `edits` arg (a single edit object or an array)
// into validated, sorted lineEdits.
func parseLineEdits(vm *goja.Runtime, v goja.Value) ([]lineEdit, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, errors.New("edits is required")
	}
	var raw []interface{}
	switch ev := v.Export().(type) {
	case []interface{}:
		raw = ev
	case map[string]interface{}:
		raw = []interface{}{ev}
	default:
		return nil, errors.New("edits must be an object or array of edit objects")
	}
	edits := make([]lineEdit, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("edit %d is not an object", i)
		}
		e, err := parseOneLineEdit(m)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %w", i, err)
		}
		edits = append(edits, e)
	}
	return edits, nil
}

func parseOneLineEdit(m map[string]interface{}) (lineEdit, error) {
	if a, ok := m["append"]; ok {
		s, _ := a.(string)
		return lineEdit{isAppend: true, text: s, hasText: true}, nil
	}
	e := lineEdit{}
	f, ok := numField(m, "from")
	if !ok {
		return e, errors.New("missing `from` (or `append`)")
	}
	e.from = f
	if e.from < 1 {
		return e, errors.New("`from` must be >= 1")
	}
	if c, ok := numField(m, "count"); ok {
		e.count = c
	}
	if e.count < 0 {
		return e, errors.New("`count` must be >= 0")
	}
	if t, ok := m["text"]; ok {
		s, _ := t.(string)
		e.text, e.hasText = s, true
	}
	if e.count == 0 && !e.hasText {
		return e, errors.New("an insert (count 0) needs `text`")
	}
	return e, nil
}

func numField(m map[string]interface{}, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

// validateLineEdits sorts edits in place (by `from`; inserts before ranges at
// the same line; appends last) and rejects overlapping ranges, so the
// single-pass applier is unambiguous.
func validateLineEdits(edits []lineEdit) error {
	sort.SliceStable(edits, func(a, b int) bool {
		ea, eb := edits[a], edits[b]
		if ea.isAppend != eb.isAppend {
			return !ea.isAppend // appends sort last
		}
		if ea.isAppend {
			return false
		}
		if ea.from != eb.from {
			return ea.from < eb.from
		}
		return ea.count < eb.count // insert (0) before range (>0) at same line
	})
	rangeEnd := 0 // exclusive upper bound of the last range
	for _, e := range edits {
		if e.isAppend {
			continue
		}
		if e.from < rangeEnd {
			return fmt.Errorf("overlapping edits near line %d", e.from)
		}
		if e.count > 0 {
			rangeEnd = e.from + e.count
		}
	}
	return nil
}

// --- fileSed: a pragmatic sed subset ---

const (
	addrNone = iota
	addrLine
	addrRange
	addrRegex
	addrLast
)

type sedAddr struct {
	kind int
	n, m int
	re   *regexp.Regexp
}

func (a sedAddr) matches(lineNo int, line string, isLast bool) bool {
	switch a.kind {
	case addrLine:
		return lineNo == a.n
	case addrRange:
		return lineNo >= a.n && lineNo <= a.m
	case addrRegex:
		return a.re.MatchString(line)
	case addrLast:
		return isLast
	default:
		return true
	}
}

// ends reports whether this line is the address's last matched line — where a
// `c` (change) over a range emits its replacement once.
func (a sedAddr) ends(lineNo int, isLast bool) bool {
	switch a.kind {
	case addrRange:
		return lineNo == a.m
	case addrLast:
		return isLast
	default:
		return true
	}
}

type sedCommand struct {
	addr   sedAddr
	cmd    byte // s d c i a
	re     *regexp.Regexp
	repl   string
	global bool
	text   string
}

func (r *run) sed(ctx context.Context, src, script, dst string) (*transformResult, error) {
	cmds, err := parseSedScript(script)
	if err != nil {
		return nil, err
	}
	return r.applyEdit(ctx, src, dst, sedFunc(cmds))
}

func sedFunc(cmds []sedCommand) streamFunc {
	return func(dst io.Writer, src io.Reader) error {
		w := bufio.NewWriter(dst)
		sc := newFileScanner(src)
		if !sc.Scan() {
			return sc.Err()
		}
		lineNo := 0
		for {
			cur := sc.Text()
			haveNext := sc.Scan()
			lineNo++
			isLast := !haveNext

			var before, after []string
			deleted, changed := false, false
			var changeText string
			for _, c := range cmds {
				if !c.addr.matches(lineNo, cur, isLast) {
					continue
				}
				switch c.cmd {
				case 'i':
					before = append(before, c.text+"\n")
				case 'a':
					after = append(after, c.text+"\n")
				case 's':
					cur = subst(c.re, cur, c.repl, c.global)
				case 'd':
					deleted = true
				case 'c':
					deleted = true
					if c.addr.ends(lineNo, isLast) {
						changed, changeText = true, c.text+"\n"
					}
				}
			}
			for _, b := range before {
				if _, err := io.WriteString(w, b); err != nil {
					return err
				}
			}
			if changed {
				if _, err := io.WriteString(w, changeText); err != nil {
					return err
				}
			} else if !deleted {
				if _, err := io.WriteString(w, cur); err != nil {
					return err
				}
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
			}
			for _, a := range after {
				if _, err := io.WriteString(w, a); err != nil {
					return err
				}
			}
			if !haveNext {
				break
			}
		}
		if err := sc.Err(); err != nil {
			return scanErr(err)
		}
		return w.Flush()
	}
}

// subst applies a substitution: all matches when global, else the first.
func subst(re *regexp.Regexp, s, repl string, global bool) string {
	if global {
		return re.ReplaceAllString(s, repl)
	}
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + string(re.ExpandString(nil, repl, s, loc)) + s[loc[1]:]
}

func parseSedScript(script string) ([]sedCommand, error) {
	var cmds []sedCommand
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimLeft(line, " \t")
		if line == "" || line[0] == '#' {
			continue
		}
		c, err := parseSedCommand(line)
		if err != nil {
			return nil, fmt.Errorf("sed %q: %w", line, err)
		}
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil, errors.New("empty sed script")
	}
	return cmds, nil
}

func parseSedCommand(line string) (sedCommand, error) {
	var c sedCommand
	addr, i, err := parseSedAddr(line)
	if err != nil {
		return c, err
	}
	c.addr = addr
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return c, errors.New("missing command")
	}
	c.cmd = line[i]
	i++
	switch c.cmd {
	case 's':
		re, repl, global, err := parseSubst(line[i:])
		if err != nil {
			return c, err
		}
		c.re, c.repl, c.global = re, repl, global
	case 'd':
		// no args
	case 'c', 'i', 'a':
		c.text = sedText(line[i:])
	default:
		return c, fmt.Errorf("unknown command %q (supported: s d c i a)", string(c.cmd))
	}
	return c, nil
}

// parseSedAddr parses an optional leading address; returns the address and the
// index where the command begins.
func parseSedAddr(line string) (sedAddr, int, error) {
	i := 0
	if i >= len(line) {
		return sedAddr{}, 0, nil
	}
	switch {
	case line[i] == '$':
		return sedAddr{kind: addrLast}, i + 1, nil
	case line[i] >= '0' && line[i] <= '9':
		n, j := readInt(line, i)
		if j < len(line) && line[j] == ',' {
			m, k := readInt(line, j+1)
			if k == j+1 {
				return sedAddr{}, 0, errors.New("bad range address")
			}
			return sedAddr{kind: addrRange, n: n, m: m}, k, nil
		}
		return sedAddr{kind: addrLine, n: n}, j, nil
	case line[i] == '/':
		pat, j, err := readDelimited(line, i+1, '/')
		if err != nil {
			return sedAddr{}, 0, err
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return sedAddr{}, 0, fmt.Errorf("address regex: %w", err)
		}
		return sedAddr{kind: addrRegex, re: re}, j, nil
	default:
		return sedAddr{kind: addrNone}, i, nil
	}
}

func readInt(s string, i int) (int, int) {
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	n, _ := strconv.Atoi(s[i:j])
	return n, j
}

// readDelimited reads up to the next unescaped delim, unescaping `\delim`.
// Returns the content and the index just past the closing delim.
func readDelimited(s string, i int, delim byte) (string, int, error) {
	var b strings.Builder
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == delim {
			b.WriteByte(delim)
			i += 2
			continue
		}
		if s[i] == delim {
			return b.String(), i + 1, nil
		}
		b.WriteByte(s[i])
		i++
	}
	return "", 0, fmt.Errorf("missing closing %q", string(delim))
}

// parseSubst parses the `s` body starting at its delimiter: <delim>re<delim>repl<delim>flags.
func parseSubst(spec string) (*regexp.Regexp, string, bool, error) {
	if spec == "" {
		return nil, "", false, errors.New("empty s command")
	}
	delim := spec[0]
	pat, i, err := readDelimited(spec, 1, delim)
	if err != nil {
		return nil, "", false, err
	}
	repl, j, err := readDelimited(spec, i, delim)
	if err != nil {
		return nil, "", false, err
	}
	global, icase := false, false
	for _, f := range spec[j:] {
		switch f {
		case 'g':
			global = true
		case 'i':
			icase = true
		default:
			return nil, "", false, fmt.Errorf("unknown s flag %q", string(f))
		}
	}
	if icase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, "", false, fmt.Errorf("s regex: %w", err)
	}
	return re, repl, global, nil
}

// sedText extracts the text argument for c/i/a: drop one leading `\` or space.
func sedText(s string) string {
	if strings.HasPrefix(s, "\\") || strings.HasPrefix(s, " ") {
		return s[1:]
	}
	return s
}
