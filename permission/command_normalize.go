package permission

import (
	"regexp"
	"slices"
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
// file, which the parser cannot see, or run their arguments as a command in
// a way it does not validate (another user, root directory or namespace).
var opaqueCommands = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"tcsh": true, "csh": true, "ash": true, "mksh": true, "rbash": true, "pdksh": true,
	"yash": true, "posh": true, "elvish": true, "nu": true, "xonsh": true,
	"eval": true, "source": true, ".": true, "xargs": true, "sudo": true, "doas": true,
	"ionice": true, "setsid": true, "chroot": true, "flock": true, "watch": true,
	"strace": true, "ltrace": true, "busybox": true, "unbuffer": true,
	"parallel": true, "script": true, "su": true, "runuser": true,
	"nsenter": true, "unshare": true, "pkexec": true, "systemd-run": true,
	"firejail": true, "bwrap": true, "proot": true,
	// Wrappers that run their arguments as a command after options or
	// operands of their own (a CPU mask, a scheduling priority).
	"caffeinate": true, "arch": true, "taskset": true, "setarch": true,
	"chrt": true, "numactl": true, "cpulimit": true,
	// Editors run commands from options (-c, +cmd, --eval), scripts and
	// startup files.
	"vim": true, "vi": true, "nvim": true, "ex": true, "view": true,
	"gvim": true, "gview": true, "vimdiff": true, "evim": true,
	"emacs": true, "emacsclient": true, "xemacs": true, // --eval, -l, -f
}

// interpreters run a program written in their arguments or read from a file,
// and every one of them can start other commands (awk's system(), perl's
// exec, python's subprocess), so an allow rule such as "awk *" would
// otherwise vouch for anything. Versioned names (python3.12, lua5.4) count
// too; see isInterpreter.
var interpreters = map[string]bool{
	"awk": true, "gawk": true, "mawk": true, "nawk": true,
	"perl": true, "python": true, "pypy": true, "ruby": true,
	"node": true, "nodejs": true, "deno": true, "bun": true,
	"php": true, "lua": true, "luajit": true, "tclsh": true, "wish": true,
	"expect": true, "Rscript": true, "osascript": true, "pwsh": true,
	"julia": true, "gdb": true, "ed": true,
}

// versionChars may trail an interpreter's name, as in python3.12.
const versionChars = "0123456789."

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

// endOfOptions ends a command's options.
const endOfOptions = "--"

// tailSpec describes where the command a wrapper runs starts in its
// arguments, for commandTail's unvalidated guess.
type tailSpec struct {
	// valueOptions are the options whose value is the next word.
	valueOptions []string
	// operands is how many words come between the options and the command,
	// such as chroot's new root or flock's lock file.
	operands int
}

// commandTails lists the wrappers, and the opaque commands that run their
// arguments as a simple command, whose wrapped command commandTail can
// guess. Shells, su and script take the command as one string, so they
// have no entry.
var commandTails = map[string]tailSpec{
	"command": {}, "builtin": {}, "nohup": {}, "time": {},
	"exec":    {valueOptions: []string{"-a"}},
	"nice":    {valueOptions: []string{"-n", "--adjustment"}},
	"env":     {valueOptions: []string{"-u", "-C", "-S", "--unset", "--chdir", "--split-string"}},
	"timeout": {valueOptions: []string{"-s", "-k", "--signal", "--kill-after"}},
	"stdbuf":  {valueOptions: []string{"-i", "-o", "-e", "--input", "--output", "--error"}},

	"eval":     {},
	"xargs":    {valueOptions: []string{"-a", "-d", "-E", "-I", "-L", "-n", "-P", "-s"}},
	"sudo":     {valueOptions: []string{"-C", "-D", "-g", "-h", "-p", "-R", "-r", "-T", "-t", "-U", "-u"}},
	"doas":     {valueOptions: []string{"-C", "-u"}},
	"ionice":   {valueOptions: []string{"-c", "-n", "-p", "-P", "-u"}},
	"setsid":   {},
	"chroot":   {valueOptions: []string{"--userspec", "--groups"}, operands: 1},
	"flock":    {valueOptions: []string{"-w", "-E", "--timeout", "--conflict-exit-code"}, operands: 1},
	"watch":    {valueOptions: []string{"-n", "--interval"}},
	"strace":   {valueOptions: []string{"-a", "-b", "-e", "-I", "-o", "-P", "-p", "-S", "-s", "-U", "-u", "-X"}},
	"ltrace":   {valueOptions: []string{"-a", "-e", "-n", "-o", "-p", "-s", "-u"}},
	"busybox":  {},
	"unbuffer": {},
	"parallel": {valueOptions: []string{"-j", "--jobs", "-S", "--sshlogin"}},
	"runuser":  {valueOptions: []string{"-u", "-g", "-G", "--user", "--group"}},
	"nsenter":  {valueOptions: []string{"-t", "--target", "-S", "-G", "--setuid", "--setgid"}},
	"unshare":  {},

	"caffeinate":  {valueOptions: []string{"-t", "-w"}},
	"arch":        {}, // its -arm64 style options take no value
	"taskset":     {operands: 1},
	"setarch":     {operands: 1},
	"chrt":        {operands: 1},
	"systemd-run": {valueOptions: []string{"-p", "--property", "-u", "--unit", "-E", "--setenv"}},
	"pkexec":      {valueOptions: []string{"--user"}},
	"numactl":     {},
	"cpulimit":    {valueOptions: []string{"-l", "--limit", "-p", "--pid"}},
}

// stateBuiltins set variables, aliases, traps or shell options, which can
// change what a later command in the line runs (PATH, an alias for git, a
// trap's command string).
var stateBuiltins = map[string]bool{
	"export": true, "declare": true, "typeset": true, "readonly": true,
	"local": true, "read": true, "unset": true, "alias": true, "trap": true,
	"enable": true, "shopt": true, "set": true, "mapfile": true, "readarray": true,
	"let": true, // also evaluates arithmetic, which runs $(...) in subscripts
}

// stateOptionLetters are builtins that change shell state only with one
// option: printf -v assigns a variable, hash -p binds a name to a path.
var stateOptionLetters = map[string]byte{"printf": 'v', "hash": 'p'}

var (
	// looseDurationPattern accepts anything timeout might read as a
	// duration (.5, 1e1, inf); it only guesses for deny rules.
	looseDurationPattern = regexp.MustCompile(`^(([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?|inf(inity)?)[smhd]?$`)
	integerPattern       = regexp.MustCompile(`^-?[0-9]+$`)
	durationPattern      = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)
	signalPattern        = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	bufferModePattern    = regexp.MustCompile(`^(L|[0-9]+[KMGTPEZY]?B?)$`)
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
	var guesses []string
	for len(words) > 0 {
		var hadPath bool
		name, hadPath = commandName(words[0].text)
		args := words[1:]
		rest, stripped, wrapperOK := stripWrapper(name, words[0].quotedAt, args)
		ok = ok && !hadPath && wrapperOK
		if !stripped {
			break
		}
		// A wrapper whose arguments did not validate still most likely runs
		// what follows its options; give deny rules that guess as well.
		if tail := commandTail(name, args); !wrapperOK && len(tail) > 0 && len(tail) != len(rest) {
			guesses = append(guesses, guessSegments(tail)...)
		}
		// time and reserved words come before assignments, not after them.
		if name == "time" || (reservedWords[name] && words[0].quotedAt == notQuoted) {
			rest, assigned = dropAssignments(rest)
			ok = ok && !assigned
		}
		words = rest
	}
	if len(words) == 0 {
		return guesses, ok
	}
	if opaqueCommands[name] {
		guesses = append(guesses, guessSegments(commandTail(name, words[1:]))...)
	}
	ok = ok && vouchableName(name) && !changesShellState(name, words[1:]) &&
		!runsCommandThroughOptions(name, words[1:]) &&
		!(expansionSensitive[name] && anyExpands(words[1:])) &&
		!evaluatesArithmetic(name, words[1:])
	texts := []string{name}
	for _, w := range words[1:] {
		texts = append(texts, w.text)
	}
	segments = append([]string{strings.Join(texts, " ")}, guesses...)
	if name == "find" {
		for _, command := range findExecCommands(words[1:]) {
			ok = false
			nested, _ := normalizeSegment(command)
			segments = append(segments, nested...)
		}
	}
	return segments, ok
}

// guessSegments normalizes a guessed command for deny rules; whether it
// could be vouched for does not matter, as the guess never is.
func guessSegments(words []word) []string {
	segments, _ := normalizeSegment(words)
	return segments
}

// commandTail guesses the command a wrapper runs, without validating its
// arguments: what follows its leading options (and their values), its
// operands, an end-of-options marker and, for timeout, one duration and,
// for env, its assignments. It is nil for commands commandTails lacks.
func commandTail(name string, args []word) []word {
	spec, known := commandTails[name]
	if !known {
		return nil
	}
	args = skipOptions(args, spec.valueOptions)
	switch {
	case name == "timeout" && len(args) > 0 && looseDurationPattern.MatchString(args[0].text):
		args = args[1:]
	case name == "env":
		args, _ = dropAssignments(args)
	}
	if len(args) < spec.operands {
		return nil
	}
	args = args[spec.operands:]
	if len(args) > 0 && args[0].text == endOfOptions {
		args = args[1:]
	}
	return args
}

// skipOptions drops leading options, the values of valueOptions, and an
// end-of-options marker.
func skipOptions(args []word, valueOptions []string) []word {
	for startsWithOption(args) {
		opt := args[0].text
		args = args[1:]
		if opt == endOfOptions {
			break
		}
		if takesValue(opt, valueOptions) && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}

// takesValue reports whether opt is one of valueOptions, or an abbreviation
// of a long one, without an attached "=value".
func takesValue(opt string, valueOptions []string) bool {
	if strings.Contains(opt, "=") {
		return false
	}
	for _, v := range valueOptions {
		if opt == v || (strings.HasPrefix(opt, "--") && strings.HasPrefix(v, opt)) {
			return true
		}
	}
	return false
}

// changesShellState reports whether a command sets variables, aliases,
// traps or other shell state that changes what later commands run.
func changesShellState(name string, args []word) bool {
	if stateBuiltins[name] {
		return true
	}
	letter, ok := stateOptionLetters[name]
	if !ok {
		return false
	}
	for _, a := range args {
		if a.text == endOfOptions || !strings.HasPrefix(a.text, "-") {
			return false
		}
		if !strings.HasPrefix(a.text, "--") && strings.IndexByte(a.text[1:], letter) >= 0 {
			return true
		}
	}
	return false
}

// expansionSensitive are the commands whose safety depends on reading their
// options: an argument that expands (${u:--c}, {-c,x}, a glob) can hide one.
var expansionSensitive = func() map[string]bool {
	m := map[string]bool{"find": true, "hash": true, "printf": true, "test": true, "[": true}
	for name := range commandOptionChecks {
		m[name] = true
	}
	return m
}()

// arithmeticTestOperators make [[ evaluate both operands as arithmetic.
var arithmeticTestOperators = map[string]bool{
	"-eq": true, "-ne": true, "-lt": true, "-le": true, "-gt": true, "-ge": true,
}

// setVariableTest is the test operator that checks whether a variable is
// set; given NAME[sub], it evaluates sub as arithmetic.
const setVariableTest = "-v"

// evaluatesArithmetic reports whether a test command evaluates one of its
// arguments as arithmetic, which runs any $(...) in an array subscript, even
// one that was quoted or came from a variable ([[ 'a[$(cmd)]' -eq 0 ]],
// test -v "$_"). A [[ that does not end its segment with ]] had a && or ||
// inside it, so its words were split into commands that are not what runs.
func evaluatesArithmetic(name string, args []word) bool {
	if name != "[[" && name != "[" && name != "test" {
		return false
	}
	if name == "[[" && (len(args) == 0 || args[len(args)-1].text != "]]" || args[len(args)-1].quotedAt != notQuoted) {
		return true
	}
	return slices.ContainsFunc(args, func(a word) bool {
		return a.text == setVariableTest || (name == "[[" && arithmeticTestOperators[a.text])
	})
}

// anyExpands reports whether any word may expand into other words.
func anyExpands(words []word) bool {
	for _, w := range words {
		if w.expands {
			return true
		}
	}
	return false
}

// vouchableName reports whether a command word names one known command.
func vouchableName(name string) bool {
	// A leading '-' is a wrapper option the wrapper did not consume.
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, blankChars) {
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
			rest := args[2:]
			return rest, true, integerPattern.MatchString(args[1].text) && !startsWithOption(rest)
		}
		return args, true, !startsWithOption(args)
	case name == "env":
		return stripEnvArgs(args)
	case name == "timeout":
		rest, ok := stripOptions(args, timeoutOptions)
		if len(rest) == 0 || !durationPattern.MatchString(rest[0].text) {
			return rest, true, false
		}
		return rest[1:], true, ok && !startsWithOption(rest[1:])
	case name == "stdbuf":
		rest, ok := stripOptions(args, stdbufOptions)
		return rest, true, ok
	}
	return nil, false, !runsHiddenCode(name)
}

// runsHiddenCode reports whether a command runs code the parser cannot see:
// an opaque command or a script interpreter.
func runsHiddenCode(name string) bool {
	return opaqueCommands[name] || isInterpreter(name)
}

// isInterpreter reports whether name is a script interpreter, with or without
// a version suffix.
func isInterpreter(name string) bool {
	return interpreters[name] || interpreters[strings.TrimRight(name, versionChars)]
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

// isAssignment reports whether w is NAME=value, NAME+=value, NAME[sub]=value
// or NAME[sub]+=value with an unquoted NAME=.
func isAssignment(w word) bool {
	eq := strings.IndexByte(w.text, '=')
	if eq <= 0 || (w.quotedAt != notQuoted && w.quotedAt <= eq) {
		return false
	}
	name := strings.TrimSuffix(w.text[:eq], "+")
	if open := strings.IndexByte(name, '['); open >= 0 && strings.HasSuffix(name, "]") {
		name = name[:open]
	}
	return isShellName(name)
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
