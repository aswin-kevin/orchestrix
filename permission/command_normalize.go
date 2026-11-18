package permission

import (
	"regexp"
	"strings"
)

// unsafeCommandChars make the command word itself depend on expansion
// (variables, globs, brace expansion), so the command that runs cannot be
// known from the text.
const unsafeCommandChars = "$*?[{"

// blankChars in an unquoted command word mean it was quoted as one word
// ("./npm test"), so it does not name the command its text suggests.
const blankChars = " \t\n"

// testCommands are the test builtins. Their names contain '[' but cannot be
// glob patterns (the bracket is never closed), so they are literal.
var testCommands = map[string]bool{"[": true, "[[": true}

// opaqueCommands run code taken from their arguments, from input or from a
// file, which the parser cannot see.
var opaqueCommands = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"eval": true, "source": true, ".": true, "xargs": true, "sudo": true, "doas": true,
}

// wrapperCommands run the rest of their words as a command. nice, env,
// timeout and stdbuf are wrappers too but take arguments of their own; see
// stripWrapper.
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

// findExecActions are the find actions that run a command per file.
var findExecActions = map[string]bool{"-exec": true, "-execdir": true, "-ok": true, "-okdir": true}

// findExecEnd lists the words that end a find -exec command.
var findExecEnd = map[string]bool{";": true, "+": true}

var (
	integerPattern    = regexp.MustCompile(`^-?[0-9]+$`)
	durationPattern   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)
	signalPattern     = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	bufferModePattern = regexp.MustCompile(`^(L|[0-9]+[KMGTPEZY]?B?)$`)
)

// optionSpec maps a wrapper's options to the check their value must pass;
// a nil check means the option takes no value.
type optionSpec map[string]func(string) bool

var timeoutOptions = optionSpec{
	"--foreground": nil, "--preserve-status": nil, "-v": nil, "--verbose": nil,
	"-s": signalPattern.MatchString, "--signal": signalPattern.MatchString,
	"-k": durationPattern.MatchString, "--kill-after": durationPattern.MatchString,
}

var stdbufOptions = optionSpec{
	"-i": bufferModePattern.MatchString, "--input": bufferModePattern.MatchString,
	"-o": bufferModePattern.MatchString, "--output": bufferModePattern.MatchString,
	"-e": bufferModePattern.MatchString, "--error": bufferModePattern.MatchString,
}

// normalizeSegment turns one simple command into the texts patterns match:
// the command itself and, for find, the commands its -exec actions run. ok
// is false when the command cannot be vouched for.
func normalizeSegment(words []word) (segments []string, ok bool) {
	words, assigned := dropAssignments(words)
	ok = !assigned
	var name string
	for len(words) > 0 {
		var hadPath bool
		name, hadPath = commandName(words[0].text)
		rest, stripped, wrapperOK := stripWrapper(name, words[0].quotedAt, words[1:])
		ok = ok && !hadPath && wrapperOK
		if !stripped {
			break
		}
		words = rest
	}
	if len(words) == 0 {
		return nil, ok
	}
	ok = ok && vouchableName(name)
	texts := []string{name}
	for _, w := range words[1:] {
		texts = append(texts, w.text)
	}
	segments = []string{strings.Join(texts, " ")}
	if name == "find" {
		for _, command := range findExecCommands(words[1:]) {
			ok = false
			nested, _ := normalizeSegment(command)
			segments = append(segments, nested...)
		}
	}
	return segments, ok
}

// vouchableName reports whether a command word names one known command.
func vouchableName(name string) bool {
	if name == "" || strings.ContainsAny(name, blankChars) {
		return false
	}
	return testCommands[name] || !strings.ContainsAny(name, unsafeCommandChars)
}

// commandName reduces a command path to its base name; hadPath reports that
// it was a path, which may name any program.
func commandName(s string) (name string, hadPath bool) {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:], true
	}
	return s, false
}

// stripWrapper removes a leading reserved word or wrapper command named name
// (with its own arguments) from a command; args are the words after name.
// stripped reports whether it removed anything; ok is false when what it saw
// cannot be vouched for: a reserved word, an opaque command, or a wrapper
// argument it does not understand.
func stripWrapper(name string, quotedAt int, args []word) (rest []word, stripped, ok bool) {
	switch {
	case reservedWords[name] && quotedAt == notQuoted:
		return args, true, false
	case wrapperCommands[name]:
		return args, true, !startsWithOption(args)
	case name == "nice":
		if len(args) >= 2 && args[0].text == "-n" {
			return args[2:], true, integerPattern.MatchString(args[1].text)
		}
		return args, true, !startsWithOption(args)
	case name == "env":
		return stripEnvArgs(args)
	case name == "timeout":
		rest, ok := stripOptions(args, timeoutOptions)
		if len(rest) == 0 || !durationPattern.MatchString(rest[0].text) {
			return rest, true, false
		}
		return rest[1:], true, ok
	case name == "stdbuf":
		rest, ok := stripOptions(args, stdbufOptions)
		return rest, true, ok
	}
	return nil, false, !opaqueCommands[name]
}

// stripEnvArgs drops env's -i and NAME=value arguments. An assignment is not
// vouched for (env PATH=. npm runs ./npm), nor is any other option (env -S
// splits a string into a command line).
func stripEnvArgs(args []word) (rest []word, stripped, ok bool) {
	ok = true
	for len(args) > 0 && (args[0].text == "-i" || isAssignment(args[0])) {
		ok = ok && args[0].text == "-i"
		args = args[1:]
	}
	return args, true, ok && !startsWithOption(args)
}

// stripOptions drops the leading options of a wrapper, with their values.
// ok is false on an option spec does not know or a value its check rejects.
func stripOptions(args []word, spec optionSpec) (rest []word, ok bool) {
	for startsWithOption(args) {
		n, valid := optionWords(args, spec)
		if !valid {
			return args, false
		}
		args = args[n:]
	}
	return args, true
}

// optionWords returns how many words the option at args[0] spans (its value
// may be attached, as in --signal=KILL or -oL, or the next word) and whether
// spec accepts it.
func optionWords(args []word, spec optionSpec) (n int, valid bool) {
	opt := args[0].text
	if name, value, found := strings.Cut(opt, "="); found && strings.HasPrefix(opt, "--") {
		check := spec[name]
		return 1, check != nil && check(value)
	}
	if check, known := spec[opt]; known {
		if check == nil {
			return 1, true
		}
		return 2, len(args) >= 2 && check(args[1].text)
	}
	if !strings.HasPrefix(opt, "--") && len(opt) > 2 {
		check := spec[opt[:2]]
		return 1, check != nil && check(opt[2:])
	}
	return 1, false
}

// findExecCommands returns the commands find's -exec style actions run: the
// words after the action up to the terminating ";" or "+".
func findExecCommands(args []word) [][]word {
	var commands [][]word
	for i := 0; i < len(args); i++ {
		if !findExecActions[args[i].text] {
			continue
		}
		end := i + 1
		for end < len(args) && !findExecEnd[args[end].text] {
			end++
		}
		commands = append(commands, args[i+1:end])
		i = end
	}
	return commands
}

func startsWithOption(words []word) bool {
	return len(words) > 0 && strings.HasPrefix(words[0].text, "-")
}

// dropAssignments drops leading NAME=value words; assigned reports whether
// there were any.
func dropAssignments(words []word) (rest []word, assigned bool) {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
		assigned = true
	}
	return words, assigned
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
