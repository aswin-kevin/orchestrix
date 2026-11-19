package permission

import "strings"

// devNull is the only file a command may redirect output to and still be
// vouched for: discarding output cannot change anything.
const devNull = "/dev/null"

// SplitCommand splits a shell command line into its simple commands, each
// normalized to the form permission patterns are written against: leading
// variable assignments and wrappers such as nohup, env or timeout are
// dropped, the command word is reduced to its base name (/bin/rm becomes rm),
// quotes are removed and words are joined with single spaces. The commands a
// find -exec action runs are added as segments of their own.
//
// The parser is deliberately conservative. ok is false whenever it cannot
// vouch that the segments are every command the line runs, and are the
// programs their names suggest: command or process substitution, grouping,
// heredocs, redirections other than discarding output to /dev/null,
// unbalanced quotes, shells, script interpreters and other commands that run
// hidden code (sh, eval, xargs, sudo, awk, python, find -exec, ...), compound
// commands, a command word that
// depends on expansion, contains whitespace or names a path (./npm), any
// variable assignment (PATH=... or LD_PRELOAD=... can change which program
// runs or what it does), and an empty line. Normalization is for deny rules:
// /bin/rm x still reads as rm x, but never vouches for the line. Even when ok
// is false the segments are a best effort that deny rules can still match.
func SplitCommand(cmd string) (segments []string, ok bool) {
	p := commandParser{src: cmd, quotedAt: notQuoted}
	p.parse()
	ok = !p.unsafe
	for _, words := range p.segments {
		normalized, segmentOK := normalizeSegment(words)
		ok = ok && segmentOK
		segments = append(segments, normalized...)
	}
	return segments, ok && len(segments) > 0
}

// notQuoted is word.quotedAt for a word with no quoted characters.
const notQuoted = -1

// word is one shell word with its quotes removed.
type word struct {
	text string
	// quotedAt is the offset in text of the first character that was quoted
	// or escaped, or notQuoted. Quoting turns a reserved word, a brace or an
	// assignment back into an ordinary word.
	quotedAt int
}

// commandParser tokenizes a command line into segments of words. It never
// stops early: on input it cannot vouch for it sets unsafe and carries on, so
// that deny rules still get a best-effort split.
type commandParser struct {
	src      string
	pos      int
	unsafe   bool
	segments [][]word
	current  []word

	// The word being built.
	buf      strings.Builder
	inWord   bool
	quotedAt int
}

func (p *commandParser) parse() {
	for p.pos < len(p.src) {
		p.step()
	}
	p.endSegment()
}

// step consumes the token at p.pos.
func (p *commandParser) step() {
	c := p.src[p.pos]
	switch {
	case c == ' ' || c == '\t':
		p.endWord()
		p.pos++
	case c == '\n' || c == ';' || c == '|':
		p.endSegment()
		p.pos++
	case c == '&':
		p.ampersand()
	case c == '>':
		p.redirect(p.takeFD())
	case c == '<':
		// Input redirection, heredocs and process substitution.
		p.reject()
		p.endWord()
		p.pos++
	case c == '(' || c == ')' || c == '`':
		// Grouping and command substitution: what follows is another command.
		p.reject()
		p.endSegment()
		p.pos++
	case c == '$' && p.peek(1) == '(':
		p.reject()
		p.endSegment()
		p.pos += 2
	case c == '\'':
		p.singleQuoted()
	case c == '"':
		p.doubleQuoted()
	case c == '\\':
		p.escaped()
	default:
		p.inWord = true
		p.buf.WriteByte(c)
		p.pos++
	}
}

// peek returns the byte off positions ahead, or 0 past the end.
func (p *commandParser) peek(off int) byte {
	if p.pos+off < len(p.src) {
		return p.src[p.pos+off]
	}
	return 0
}

func (p *commandParser) reject() { p.unsafe = true }

// markQuoted records that the next character of the word is quoted; an empty
// pair of quotes still makes a word.
func (p *commandParser) markQuoted() {
	p.inWord = true
	if p.quotedAt == notQuoted {
		p.quotedAt = p.buf.Len()
	}
}

func (p *commandParser) endWord() {
	if !p.inWord {
		return
	}
	w := word{text: p.buf.String(), quotedAt: p.quotedAt}
	if w.quotedAt == notQuoted && (w.text == "{" || w.text == "}") {
		p.reject() // a command group
	}
	p.current = append(p.current, w)
	p.buf.Reset()
	p.inWord = false
	p.quotedAt = notQuoted
}

func (p *commandParser) endSegment() {
	p.endWord()
	if len(p.current) > 0 {
		p.segments = append(p.segments, p.current)
	}
	p.current = nil
}

// ampersand handles "&>" (redirect both streams) and otherwise "&" or "&&",
// which both end a command.
func (p *commandParser) ampersand() {
	if p.peek(1) == '>' {
		p.endWord()
		p.pos++
		p.redirect("&")
		return
	}
	p.endSegment()
	p.pos++
}

// takeFD returns the file descriptor written directly before a '>' ("" for
// none) and consumes it. Only an unquoted all-digit word is a descriptor;
// any other word just ends there, as in "echo a>/dev/null".
func (p *commandParser) takeFD() string {
	text := p.buf.String()
	if !p.inWord || p.quotedAt != notQuoted || strings.Trim(text, "0123456789") != "" {
		p.endWord()
		return ""
	}
	p.buf.Reset()
	p.inWord = false
	return text
}

// redirect consumes a redirection whose '>' is at p.pos. fd is "", a digit
// string, or "&" for both streams. Only discarding output (>/dev/null,
// 2>/dev/null, &>/dev/null) and 2>&1 are vouched for; the redirection is
// removed from the segment either way.
func (p *commandParser) redirect(fd string) {
	p.pos++
	allowed := false
	switch p.peek(0) {
	case '(':
		// Process substitution; step handles the parenthesis.
		p.reject()
		return
	case '>':
		p.pos++ // appending to a file
		p.target()
	case '&':
		p.pos++
		target := p.target()
		allowed = fd == "2" && target == "1"
	default:
		target := p.target()
		allowed = (fd == "" || fd == "2" || fd == "&") && target == devNull
	}
	if !allowed {
		p.reject()
	}
}

// target consumes the word a redirection points at, after optional blanks,
// and returns it verbatim: a quoted or escaped target never equals devNull,
// so it is rejected rather than interpreted. The scan stops before a
// backtick or '$' so that a substitution in the target is still parsed.
func (p *commandParser) target() string {
	for p.peek(0) == ' ' || p.peek(0) == '\t' {
		p.pos++
	}
	start := p.pos
	for p.pos < len(p.src) && strings.IndexByte(" \t\n;&|<>()`$", p.src[p.pos]) < 0 {
		p.pos++
	}
	// An expansion continues the target, so the file written is unknown;
	// step then splits out any command substitution for deny rules.
	if c := p.peek(0); c == '`' || c == '$' {
		p.reject()
	}
	return p.src[start:p.pos]
}

// singleQuoted consumes '...', whose content is literal.
func (p *commandParser) singleQuoted() {
	p.markQuoted()
	p.pos++
	end := strings.IndexByte(p.src[p.pos:], '\'')
	if end < 0 {
		p.reject()
		end = len(p.src) - p.pos
	}
	p.buf.WriteString(p.src[p.pos : p.pos+end])
	p.pos += end + 1
}

// doubleQuoted consumes "...", where a backslash escapes only " \ $ and `,
// and command substitution still runs.
func (p *commandParser) doubleQuoted() {
	p.markQuoted()
	p.pos++
	for p.pos < len(p.src) {
		c, next := p.src[p.pos], p.peek(1)
		switch {
		case c == '"':
			p.pos++
			return
		case c == '\\' && next != 0 && strings.IndexByte("\"\\$`", next) >= 0:
			p.buf.WriteByte(next)
			p.pos += 2
			continue
		case c == '\\' && next == '\n':
			p.pos += 2 // line continuation
			continue
		case c == '`' || (c == '$' && next == '('):
			p.reject()
		}
		p.buf.WriteByte(c)
		p.pos++
	}
	p.reject() // unterminated
}

// escaped consumes a backslash outside quotes: it quotes the next character,
// or joins lines when that character is a newline.
func (p *commandParser) escaped() {
	switch {
	case p.pos+1 >= len(p.src):
		p.reject() // a trailing backslash continues onto a line we cannot see
		p.pos++
	case p.peek(1) == '\n':
		p.pos += 2
	default:
		p.markQuoted()
		p.buf.WriteByte(p.peek(1))
		p.pos += 2
	}
}
