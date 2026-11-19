package permission

import "strings"

// commandOptionChecks hold, for ordinary commands that can also run a command
// of the caller's choosing, a check of whether their arguments make them do
// so. The command the option names is invisible to the parser, so a line
// that uses one is never vouched for: an allow rule such as git:* must not
// approve git -c core.fsmonitor='rm x' status.
var commandOptionChecks = map[string]func(args []word) bool{
	"git": gitRunsCommand,
	"sed": sedRunsCommand, "gsed": sedRunsCommand,
	"tar": tarRunsCommand, "gtar": tarRunsCommand, "bsdtar": tarRunsCommand,
}

// runsCommandThroughOptions reports whether the arguments of the command
// name make it run a command the parser cannot see.
func runsCommandThroughOptions(name string, args []word) bool {
	check, ok := commandOptionChecks[name]
	return ok && check(args)
}

// isLongOption reports whether arg is the long option opt, with or without an
// attached "=value", or an abbreviation of it, as getopt-style parsers accept.
// An ambiguous abbreviation also counts, which errs on the side of caution.
func isLongOption(arg, opt string) bool {
	name, _, _ := strings.Cut(arg, "=")
	return name == opt || (len(name) > len("--") && strings.HasPrefix(opt, name))
}

// hasShortOption reports whether arg is a cluster of short options (-xvf)
// that contains letter. Letters of an attached value count too, which errs
// on the side of caution.
func hasShortOption(arg string, letter byte) bool {
	return isShortCluster(arg) && strings.IndexByte(arg[1:], letter) >= 0
}

func isShortCluster(arg string) bool {
	return len(arg) > 1 && arg[0] == '-' && arg[1] != '-'
}

// hasAnyOption reports whether args, up to an end-of-options marker, use any
// of options: long options ("--exec") or single-letter short ones ("-x").
func hasAnyOption(args []word, options []string) bool {
	for _, a := range args {
		if a.text == endOfOptions {
			return false
		}
		for _, opt := range options {
			long := strings.HasPrefix(opt, "--")
			if (long && isLongOption(a.text, opt)) || (!long && hasShortOption(a.text, opt[1])) {
				return true
			}
		}
	}
	return false
}

// tarCommandOptions name a program tar runs: a filter for extracted files, a
// compressor, a checkpoint or volume-change action, or a remote shell.
var tarCommandOptions = []string{
	"--to-command", "--use-compress-program", "--checkpoint-action",
	"--info-script", "--new-volume-script", "--rsh-command", "--rmt-command",
	"-I", // --use-compress-program
	"-F", // --info-script
}

// tarRunsCommand reports whether tar's arguments name a program to run. The
// first argument may bundle options without a leading dash (tar xIf ...).
func tarRunsCommand(args []word) bool {
	if len(args) > 0 && !strings.HasPrefix(args[0].text, "-") &&
		strings.ContainsAny(args[0].text, "IF") {
		return true
	}
	return hasAnyOption(args, tarCommandOptions)
}

// gitValueOptions are git's global options whose value may be the next word
// (-c and --config-env are refused before their value matters).
var gitValueOptions = map[string]bool{
	"-C": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
}

// gitCommandOptions are, per subcommand, the options whose value is a command
// git runs, or that install hooks or configuration that later run one.
var gitCommandOptions = map[string][]string{
	"rebase":     {"--exec", "-x"},
	"difftool":   {"--extcmd", "-x"},
	"grep":       {"--open-files-in-pager", "-O"},
	"fetch":      {"--upload-pack"},
	"pull":       {"--upload-pack"},
	"ls-remote":  {"--upload-pack"},
	"push":       {"--receive-pack", "--exec"},
	"archive":    {"--exec"},
	"clone":      {"--upload-pack", "-u", "--config", "-c", "--template"},
	"init":       {"--template"},
	"instaweb":   {"--httpd", "-d", "--browser", "-b"},
	"send-email": {"--sendmail-cmd", "--smtp-server", "--to-cmd", "--cc-cmd", "--header-cmd"},
}

// gitCommandActions are the subcommands that run a command given as their
// arguments when used with the action word: git bisect run, git submodule
// foreach.
var gitCommandActions = map[string]string{"bisect": "run", "submodule": "foreach"}

// gitConfigReads are the git config actions that only read; any other use
// may set an alias, hook path or core.fsmonitor that a later git command
// runs.
var gitConfigReads = []string{"--get", "--get-all", "--get-regexp", "--list", "-l", "get", "list"}

// gitRunsCommand reports whether git's arguments make it run a command of
// the caller's choosing: global configuration (-c, --config-env), a
// replacement for its own programs (--exec-path=), or a subcommand option or
// action that runs or installs one.
func gitRunsCommand(args []word) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i].text
		switch {
		case strings.HasPrefix(arg, "-c") || strings.HasPrefix(arg, "--config-env") ||
			strings.HasPrefix(arg, "--exec-path="):
			return true
		case gitValueOptions[arg]:
			i++
		case !strings.HasPrefix(arg, "-"):
			return gitSubcommandRunsCommand(arg, args[i+1:])
		}
	}
	return false
}

func gitSubcommandRunsCommand(subcommand string, args []word) bool {
	if subcommand == "filter-branch" {
		return true // every filter is a shell command
	}
	if subcommand == "config" {
		return !hasAnyWord(args, gitConfigReads)
	}
	if action, ok := gitCommandActions[subcommand]; ok && hasAnyWord(args, []string{action}) {
		return true
	}
	return hasAnyOption(args, gitCommandOptions[subcommand])
}

// hasAnyWord reports whether args, up to an end-of-options marker, contain
// one of words exactly.
func hasAnyWord(args []word, words []string) bool {
	for _, a := range args {
		if a.text == endOfOptions {
			return false
		}
		for _, w := range words {
			if a.text == w {
				return true
			}
		}
	}
	return false
}
