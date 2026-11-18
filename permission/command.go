package permission

import "strings"

// devNull is the only file a command may redirect output to and still be
// vouched for: discarding output cannot change anything.
const devNull = "/dev/null"

// unsafeCommandChars make the command word itself depend on expansion
// (variables, globs, brace expansion), so the command that runs cannot be
// known from the text.
const unsafeCommandChars = "$*?[{"

// testCommands are the test builtins. Their names contain '[' but cannot be
// glob patterns (the bracket is never closed), so they are literal.
var testCommands = map[string]bool{"[": true, "[[": true}

// opaqueCommands run code taken from their arguments, from input or from a
// file, which the parser cannot see.
var opaqueCommands = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"eval": true, "source": true, ".": true, "xargs": true, "sudo": true, "doas": true,
}

// wrapperCommands run the rest of their words as a command. nice and env are
// wrappers too but take arguments of their own; see stripWrapper.
var wrapperCommands = map[string]bool{
	"command": true, "builtin": true, "nohup": true, "time": true, "exec": true,
}

// reservedWords introduce compound commands, whose control flow the parser
// does not follow. They are dropped so deny rules still see the commands
// inside, but a line containing one is never vouched for.
var reservedWords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"do": true, "done": true, "case": true, "esac": true, "while": true,
	"until": true, "for": true, "select": true, "function": true,
	"coproc": true, "!": true,
}

// SplitCommand splits a shell command line into its simple commands, each
// normalized to the form permission patterns are written against: leading
// variable assignments and wrappers such as nohup or env are dropped, the
// command word is reduced to its base name (/bin/rm becomes rm), quotes are
// removed and words are joined with single spaces.
//
// The parser is deliberately conservative. ok is false whenever it cannot
// vouch that the segments are every command the line runs: command or process
// substitution, grouping, heredocs, redirections other than discarding output
// to /dev/null, unbalanced quotes, shells and other commands that run hidden
// code (sh, eval, xargs, sudo, ...), compound commands, a command word that
// depends on expansion, and an empty line. Even then the segments are a best
// effort that deny rules can still match.
func SplitCommand(cmd string) (segments []string, ok bool) {
	p := commandParser{src: cmd, quotedAt: notQuoted}
	p.parse()
	ok = !p.unsafe
	for _, words := range p.segments {
		segment, segmentOK := normalizeSegment(words)
		ok = ok && segmentOK
		if segment != "" {
			segments = append(segments, segment)
		}
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
// so it is rejected rather than interpreted.
func (p *commandParser) target() string {
	for p.peek(0) == ' ' || p.peek(0) == '\t' {
		p.pos++
	}
	start := p.pos
	for p.pos < len(p.src) && strings.IndexByte(" \t\n;&|<>()", p.src[p.pos]) < 0 {
		p.pos++
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

// normalizeSegment turns one simple command into the text patterns match.
// ok is false when the command cannot be vouched for.
func normalizeSegment(words []word) (segment string, ok bool) {
	ok = true
	words = dropAssignments(words)
	for len(words) > 0 {
		words[0].text = commandName(words[0].text)
		rest, stripped, wrapperOK := stripWrapper(words)
		ok = ok && wrapperOK
		if !stripped {
			break
		}
		words = rest
	}
	if len(words) == 0 {
		return "", ok
	}
	if !vouchableName(words[0].text) {
		ok = false
	}
	texts := make([]string, len(words))
	for i, w := range words {
		texts[i] = w.text
	}
	return strings.Join(texts, " "), ok
}

// vouchableName reports whether a command word names one known command.
func vouchableName(name string) bool {
	return name != "" && (testCommands[name] || !strings.ContainsAny(name, unsafeCommandChars))
}

// commandName reduces a command path to its base name.
func commandName(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// stripWrapper removes a leading reserved word or wrapper command (with its
// own arguments) from words. stripped reports whether it removed anything;
// ok is false when what it saw cannot be vouched for: a reserved word, an
// opaque command, or a wrapper option it does not understand.
func stripWrapper(words []word) (rest []word, stripped, ok bool) {
	name, args := words[0].text, words[1:]
	switch {
	case reservedWords[name] && words[0].quotedAt == notQuoted:
		return args, true, false
	case wrapperCommands[name]:
		return args, true, !startsWithOption(args)
	case name == "nice":
		if len(args) >= 2 && args[0].text == "-n" {
			return args[2:], true, true
		}
		return args, true, !startsWithOption(args)
	case name == "env":
		for len(args) > 0 && (args[0].text == "-i" || isAssignment(args[0])) {
			args = args[1:]
		}
		// Any other option may change what runs: env -S splits a string
		// into a command line.
		return args, true, !startsWithOption(args)
	}
	return words, false, !opaqueCommands[name]
}

func startsWithOption(words []word) bool {
	return len(words) > 0 && strings.HasPrefix(words[0].text, "-")
}

func dropAssignments(words []word) []word {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	return words
}

// isAssignment reports whether w is NAME=value with an unquoted NAME=.
func isAssignment(w word) bool {
	eq := strings.IndexByte(w.text, '=')
	if eq <= 0 || (w.quotedAt != notQuoted && w.quotedAt <= eq) {
		return false
	}
	return isShellName(w.text[:eq])
}

// isShellName reports whether s is a valid shell variable name.
func isShellName(s string) bool {
	for i, c := range s {
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		digit := c >= '0' && c <= '9'
		if !letter && (i == 0 || !digit) {
			return false
		}
	}
	return s != ""
}
