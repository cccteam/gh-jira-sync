package ghjira

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// apiFactory builds the API client for a validated config; tests pass a fake.
type apiFactory func(*Config) (API, error)

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)

	return nil
}

// locked holds an exclusive advisory lock on path until the returned func is called.
func locked(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, syncErr("Another process holds the state lock")
		}

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return func() {
		// Closing the file releases the lock even if the explicit unlock fails.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

type report struct {
	Mode           string   `json:"mode"`
	Entries        []*Entry `json:"entries"`
	PlannedWrites  *int     `json:"planned_writes,omitempty"`
	DeferredWrites *int     `json:"deferred_writes,omitempty"`
	Error          string   `json:"error,omitempty"`
	ExitCode       int      `json:"exit_code"`
}

func count(entries []*Entry, match func(*Entry) bool) *int {
	n := 0
	for _, e := range entries {
		if match(e) {
			n++
		}
	}

	return &n
}

// options are the parsed command-line flags.
type options struct {
	configPath, statePath, reportPath, gitBranch, gitRef string
	apply, initState                                     bool
	acknowledge                                          stringList
}

// parseFlags returns the options, or an exit code when the run should stop here.
func parseFlags(args []string, stdout io.Writer) (*options, int, bool) {
	o := &options{}
	flags := flag.NewFlagSet("gh-jira-sync", flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), "Syncs GitHub and Jira issues: creation, comments, status, and parents")
		flags.PrintDefaults()
	}
	flags.StringVar(&o.configPath, "config", "sync.json", "configuration file")
	flags.StringVar(&o.statePath, "state", ".sync-state/state.json", "local state file (a checkpoint copy in Git mode)")
	flags.StringVar(&o.reportPath, "report", "sync-report.json", "report file")
	flags.StringVar(&o.gitBranch, "git-state-branch", "", "persist every intent to this `branch` on origin (sync-state or sync-state-<name>) before API writes; required for ephemeral runners")
	flags.StringVar(&o.gitRef, "git-state-ref", "", "like -git-state-branch, but keep state on the hidden ref refs/gh-jira-sync/`name` (sync-state or sync-state-<name>), which the branch list and clones do not show")
	flags.BoolVar(&o.apply, "apply", false, "perform remote writes (otherwise dry-run)")
	flags.BoolVar(&o.initState, "init-state", false, "permit a new journal; never use to replace lost production state")
	flags.Var(&o.acknowledge, "acknowledge", "silence a reviewed blocked/unavailable `OPERATION` until its reason changes (saved with -apply; repeatable)")
	version := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0, false
		}

		return nil, 2, false
	}
	if *version {
		_, _ = fmt.Fprintln(stdout, "gh-jira-sync", Version())

		return nil, 0, false
	}
	if o.gitBranch != "" && o.gitRef != "" {
		_, _ = fmt.Fprintln(flags.Output(), "use -git-state-branch or -git-state-ref, not both")

		return nil, 2, false
	}
	for flagName, v := range map[string]string{"-git-state-branch": o.gitBranch, "-git-state-ref": o.gitRef} {
		if v != "" && !stateBranchPattern.MatchString(v) {
			_, _ = fmt.Fprintln(flags.Output(), flagName, "must be sync-state or sync-state-<name>")

			return nil, 2, false
		}
	}

	return o, 0, true
}

// Main runs the CLI and returns the exit code: 0 success, 2 needs review, 1 error.
func Main(args []string, stdout io.Writer) int {
	return mainWith(args, stdout, func(c *Config) (API, error) { return NewClient(c) })
}

func mainWith(args []string, stdout io.Writer, newAPI apiFactory) int {
	o, code, ok := parseFlags(args, stdout)
	if !ok {
		return code
	}
	mode := "dry-run"
	if o.apply {
		mode = "apply"
	}
	rep := &report{Mode: mode, Entries: []*Entry{}}
	paths := map[string]bool{}
	for _, p := range []string{o.configPath, o.statePath, o.reportPath, o.statePath + ".lock"} {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		paths[abs] = true
	}
	distinct := len(paths) == 4
	code, err := func() (code int, err error) {
		defer func() {
			// Malformed input must still produce a report rather than a stack trace.
			if r := recover(); r != nil {
				code, err = 1, fmt.Errorf("panic: %v", r)
			}
		}()
		if !distinct {
			return 1, syncErr("Config, state, report, and lock paths must differ")
		}

		return run(o, rep, newAPI)
	}()
	if err != nil {
		code, rep.Error = 1, reportError(err)
	}
	rep.ExitCode = code
	if distinct {
		if err := atomicJSON(o.reportPath, rep); err != nil {
			rep.Error = "Could not persist report"
			code, rep.ExitCode = 1, 1
		}
	}
	_, _ = fmt.Fprintln(stdout, string(canonical(rep, true)))

	return code
}

// run loads config and state, plans, and applies when asked, filling rep. It returns
// 0 when nothing needs review and 2 when something does.
func run(o *options, rep *report, newAPI apiFactory) (int, error) {
	data, err := os.ReadFile(o.configPath)
	if err != nil {
		return 1, fmt.Errorf("read config: %w", err)
	}
	c, err := ParseConfig(data)
	if err != nil {
		return 1, err
	}
	unlock, err := locked(o.statePath + ".lock")
	if err != nil {
		return 1, err
	}
	defer unlock()
	store, state, err := loadState(o, c)
	if err != nil {
		return 1, err
	}
	api, err := newAPI(c)
	if err != nil {
		return 1, err
	}
	entries, err := Plan(c, state, api, time.Now())
	if err != nil {
		return 1, err
	}
	if state, err = Acknowledge(state, entries, o.acknowledge); err != nil {
		return 1, err
	}
	rep.Entries = entries // entries are pointers, so apply outcomes appear in the report
	rep.PlannedWrites = count(entries, func(e *Entry) bool { return e.Kind.writes() })
	rep.DeferredWrites = count(entries, func(e *Entry) bool { return e.Kind == KindDeferred })
	if o.apply {
		save := func(s *State) error {
			// Remote is authoritative; losing local disk must not lose an intent.
			if store != nil {
				if err := store.Save(s); err != nil {
					return err
				}
			}

			return atomicJSON(o.statePath, s)
		}
		if _, err := Apply(c, state, api, entries, save); err != nil {
			return 1, err
		}
	}
	for _, e := range entries {
		if e.Kind.needsReview() || e.Outcome == OutcomeRejected {
			return 2, nil
		}
	}

	return 0, nil
}

// loadState reads state from the Git branch or hidden ref, or the local file; a missing journal is
// created only when -init-state allows it.
func loadState(o *options, c *Config) (*GitStore, *State, error) {
	var store *GitStore
	var state *State
	var err error
	if o.gitBranch != "" || o.gitRef != "" {
		if o.gitRef != "" {
			store, err = NewHiddenGitStore(o.gitRef)
		} else {
			store, err = NewGitStore(o.gitBranch)
		}
		if err != nil {
			return nil, nil, err
		}
		state, err = store.Load()
	} else if raw, readErr := os.ReadFile(o.statePath); readErr == nil {
		state, err = ParseState(raw)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		err = fmt.Errorf("read state: %w", readErr)
	}
	if err != nil {
		return nil, nil, err
	}
	if state == nil {
		if !o.initState {
			return nil, nil, syncErr("State missing; restore it or explicitly initialize a new deployment")
		}
		state = NewState(c)
	}

	return store, state, nil
}

// reportError renders an error for the report without quoting file contents.
func reportError(err error) string {
	var pathErr *fs.PathError
	switch {
	case isSyncError(err):
		return err.Error()
	case errors.As(err, &pathErr):
		// Holds only the operation, path, and OS reason, such as "no such file or directory".
		return "Cannot " + pathErr.Op + " " + pathErr.Path + ": " + pathErr.Err.Error()
	default:
		// Other errors, such as JSON syntax errors, may quote file contents; report only
		// the type of the underlying error.
		for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(err) {
			err = inner
		}

		return fmt.Sprintf("Invalid input or I/O failure (%T)", err)
	}
}
