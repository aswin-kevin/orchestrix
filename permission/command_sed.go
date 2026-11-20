package permission

import "strings"

// sedLongOptions are GNU sed's long options, mapped to whether they take a
// value that may be the next word. --in-place takes its suffix only attached.
var sedLongOptions = map[string]bool{
	"--expression": true, "--file": true, "--line-length": true,
	"--in-place": false, "--quiet": false, "--silent": false,
	"--regexp-extended": false, "--separate": false, "--unbuffered": false,
	"--null-data": false, "--zero-terminated": false, "--posix": false,
	"--debug": false, "--sandbox": false, "--follow-symlinks": false,
	"--binary": false, "--help": false, "--version": false,
}

// sedRunsCommand reports whether sed may run a command: GNU sed's e command
// and the e flag of s run one. A script read from a file is unseen, and
// arguments or scripts the parser does not understand count as running one.
func sedRunsCommand(args []word) bool {
	scripts, ok := sedScripts(args)
	// GNU sed joins its scripts with newlines before parsing them, so a
	// construct may continue from one -e into the next.
	return !ok || sedScriptRunsCommand(strings.Join(scripts, "\n"))
}

// sedScripts returns the scripts a sed command line runs: those given with
// -e or --expression or, without them, the first operand. Options may follow
// operands, as GNU sed allows. ok is false for a script file (-f), for an
// option it does not know and for a line with no script.
func sedScripts(args []word) (scripts []string, ok bool) {
	var operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i].text
		var next *string
		if i+1 < len(args) {
			next = &args[i+1].text
		}
		var script string
		var hasScript, usedNext bool
		switch {
		case arg == endOfOptions:
			for _, w := range args[i+1:] {
				operands = append(operands, w.text)
			}
			i = len(args)
			continue
		case strings.HasPrefix(arg, "--"):
			script, hasScript, usedNext, ok = sedLongOption(arg, next)
		case isShortCluster(arg):
			script, hasScript, usedNext, ok = sedShortOptions(arg, next)
		default:
			operands = append(operands, arg)
			continue
		}
		if !ok {
			return nil, false
		}
		if hasScript {
			scripts = append(scripts, script)
		}
		if usedNext {
			i++
		}
	}
	if len(scripts) == 0 && len(operands) > 0 {
		scripts = operands[:1]
	}
	return scripts, len(scripts) > 0
}

// sedLongOption parses one long option; next is the following word, if any.
// It resolves abbreviations and fails on unknown or ambiguous ones.
func sedLongOption(arg string, next *string) (script string, hasScript, usedNext, ok bool) {
	name, value, hasValue := strings.Cut(arg, "=")
	var match string
	for opt := range sedLongOptions {
		if opt == name {
			match = opt
			break
		}
		if strings.HasPrefix(opt, name) {
			if match != "" {
				return "", false, false, false // ambiguous
			}
			match = opt
		}
	}
	if match == "" || match == "--file" {
		return "", false, false, false
	}
	if sedLongOptions[match] && !hasValue {
		if next == nil {
			return "", false, false, false
		}
		value, usedNext = *next, true
	}
	return value, match == "--expression", usedNext, true
}

// sedShortOptions parses a cluster of short options such as -ne; next is the
// following word, if any. e, f and l take a value, attached or as the next
// word; i takes an optional attached backup suffix.
func sedShortOptions(arg string, next *string) (script string, hasScript, usedNext, ok bool) {
	for j := 1; j < len(arg); j++ {
		switch c := arg[j]; c {
		case 'n', 'r', 's', 'u', 'z', 'E':
		case 'i':
			return "", false, false, true
		case 'e', 'f', 'l':
			value := arg[j+1:]
			if value == "" {
				if next == nil {
					return "", false, false, false
				}
				value, usedNext = *next, true
			}
			return value, c == 'e', usedNext, c != 'f'
		default:
			return "", false, false, false
		}
	}
	return "", false, false, true
}

// sedScriptRunsCommand reports whether a sed script may run a command: an e
// command, or an s command with the e flag. It parses enough of the script
// to tell commands from addresses, regexes, text and file names; anything it
// does not recognise counts as running a command.
func sedScriptRunsCommand(script string) bool {
	s := sedScanner{src: script}
	for s.skip(" \t\n;"); !s.done(); s.skip(" \t\n;") {
		if !s.address() || !s.command() {
			return true
		}
	}
	return false
}

// sedScanner walks a sed script.
type sedScanner struct {
	src string
	pos int
}

func (s *sedScanner) done() bool { return s.pos >= len(s.src) }

// peek returns the current byte, or 0 at the end.
func (s *sedScanner) peek() byte {
	if s.done() {
		return 0
	}
	return s.src[s.pos]
}

// skip consumes any of chars.
func (s *sedScanner) skip(chars string) {
	for !s.done() && strings.IndexByte(chars, s.peek()) >= 0 {
		s.pos++
	}
}

// skipDigits consumes a run of digits.
func (s *sedScanner) skipDigits() { s.skip("0123456789") }

// address consumes an optional address or address range and any negation.
func (s *sedScanner) address() bool {
	if !s.oneAddress(false) {
		return false
	}
	s.skip(" \t")
	if s.peek() == ',' {
		s.pos++
		s.skip(" \t")
		if !s.oneAddress(true) {
			return false
		}
	}
	s.skip(" \t!")
	return true
}

// oneAddress consumes one address, if there is one: a line number (with a
// ~step), $, or a regex with its I and M flags. After a comma, +N and ~N
// are addresses too.
func (s *sedScanner) oneAddress(second bool) bool {
	switch c := s.peek(); {
	case c >= '0' && c <= '9':
		s.skipDigits()
		if s.peek() == '~' {
			s.pos++
			s.skipDigits()
		}
	case c == '$':
		s.pos++
	case second && (c == '+' || c == '~'):
		s.pos++
		s.skipDigits()
	case c == '/' || c == '\\':
		if c == '\\' {
			s.pos++ // \cREGEXc: the next character is the delimiter
		}
		if s.done() {
			return false
		}
		delim := s.src[s.pos]
		s.pos++
		if !s.through(delim) {
			return false
		}
		s.skip("IM")
	}
	return true
}

// through consumes text up to and including the next unescaped delim. It
// reports false if there is none.
func (s *sedScanner) through(delim byte) bool {
	for !s.done() {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case delim:
			s.pos++
			return true
		default:
			s.pos++
		}
	}
	return false
}

// toLineEnd consumes the rest of the line. Only text (of a, i and c)
// continues past a newline escaped with a backslash; a comment or file name
// ends at the first newline, as in GNU sed.
func (s *sedScanner) toLineEnd(text bool) {
	for !s.done() && s.peek() != '\n' {
		if text && s.peek() == '\\' {
			s.pos++
		}
		s.pos++
	}
}

// label consumes a label. It stops at the earliest point GNU sed could end
// one, so that text after it is never mistaken for part of the label.
func (s *sedScanner) label() {
	s.skip(" \t")
	for !s.done() && strings.IndexByte(" \t\n;}", s.peek()) < 0 {
		s.pos++
	}
}

// command consumes one command and its arguments. It reports false for the
// e command, the e flag of s, and anything it does not recognise.
func (s *sedScanner) command() bool {
	if s.done() {
		return false // an address without a command
	}
	c := s.src[s.pos]
	s.pos++
	switch c {
	case '{', '}', '=', 'd', 'D', 'g', 'G', 'h', 'H', 'n', 'N', 'p', 'P', 'x', 'z', 'F':
	case 'q', 'Q', 'l', 'L':
		s.skip(" \t")
		s.skipDigits()
	case 'a', 'i', 'c':
		s.toLineEnd(true)
	case '#', 'r', 'R', 'w', 'W':
		s.toLineEnd(false) // a comment or a file name
	case ':', 'b', 't', 'T', 'v':
		s.label()
	case 'y':
		return s.substitution(false)
	case 's':
		return s.substitution(true)
	default:
		return false // e, or a command this scanner does not know
	}
	return true
}

// substitution consumes the delimiter, pattern and replacement of an s or y
// command and, for s, its flags. It reports false for an e flag.
func (s *sedScanner) substitution(flags bool) bool {
	if s.done() || s.peek() == '\n' || s.peek() == '\\' {
		return false
	}
	delim := s.peek()
	s.pos++
	if !s.through(delim) { // pattern
		return false
	}
	if !s.through(delim) { // replacement
		return false
	}
	for flags && !s.done() {
		switch c := s.peek(); {
		case c == 'e':
			return false
		case c == 'w':
			s.toLineEnd(false) // the rest of the line is a file name
			return true
		case strings.IndexByte("gpiImM0123456789", c) >= 0:
			s.pos++
		default:
			return true
		}
	}
	return true
}
