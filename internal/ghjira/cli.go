package ghjira

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// newAPI is replaced in tests with a fake.
var newAPI = func(c *Config) (API, error) { return NewClient(c) }

// now is replaced in tests that need a fixed clock.
var now = time.Now

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// locked holds an exclusive advisory lock on path until the returned func is called.
func locked(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, syncErr("Another process holds the state lock")
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
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

// Main runs the CLI and returns the exit code: 0 success, 2 needs review, 1 error.
func Main(args []string, stdout io.Writer) int {
	flags := flag.NewFlagSet("gh-jira-sync", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "GitHub → Jira creation; two-way original comment creation/edits")
		flags.PrintDefaults()
	}
	configPath := flags.String("config", "sync.json", "configuration file")
	statePath := flags.String("state", ".sync-state/state.json", "local state file (a checkpoint copy in Git mode)")
	reportPath := flags.String("report", "sync-report.json", "report file")
	gitBranch := flags.String("git-state-branch", "", "persist every intent to origin's `sync-state` branch before API writes (required for ephemeral runners)")
	applyWrites := flags.Bool("apply", false, "perform remote writes (otherwise dry-run)")
	initState := flags.Bool("init-state", false, "permit a new journal; never use to replace lost production state")
	var acknowledge stringList
	flags.Var(&acknowledge, "acknowledge", "silence a reviewed blocked/unavailable `OPERATION` until its reason changes (saved with -apply; repeatable)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *gitBranch != "" && *gitBranch != "sync-state" {
		fmt.Fprintln(flags.Output(), "-git-state-branch must be sync-state")
		return 2
	}

	mode := "dry-run"
	if *applyWrites {
		mode = "apply"
	}
	rep := &report{Mode: mode, Entries: []*Entry{}}
	code := 1
	paths := map[string]bool{}
	for _, p := range []string{*configPath, *statePath, *reportPath, *statePath + ".lock"} {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		paths[abs] = true
	}
	distinct := len(paths) == 4
	err := func() (err error) {
		defer func() {
			// Malformed input must still produce a report rather than a stack trace.
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		if !distinct {
			return syncErr("Config, state, report, and lock paths must differ")
		}
		data, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		c, err := ParseConfig(data)
		if err != nil {
			return err
		}
		unlock, err := locked(*statePath + ".lock")
		if err != nil {
			return err
		}
		defer unlock()
		var store *GitStore
		var state *State
		if *gitBranch != "" {
			if store, err = NewGitStore(*gitBranch); err != nil {
				return err
			}
			state, err = store.Load()
		} else if raw, readErr := os.ReadFile(*statePath); readErr == nil {
			state, err = ParseState(raw)
		} else if !errors.Is(readErr, os.ErrNotExist) {
			err = readErr
		}
		if err != nil {
			return err
		}
		if state == nil {
			if !*initState {
				return syncErr("State missing; restore it or explicitly initialize a new deployment")
			}
			state = NewState(c)
		}
		api, err := newAPI(c)
		if err != nil {
			return err
		}
		entries, err := Plan(c, state, api, now())
		if err != nil {
			return err
		}
		if state, err = Acknowledge(state, entries, acknowledge); err != nil {
			return err
		}
		rep.Entries = entries // entries are pointers, so apply outcomes appear in the report
		rep.PlannedWrites = count(entries, func(e *Entry) bool { return e.Kind.writes() })
		rep.DeferredWrites = count(entries, func(e *Entry) bool { return e.Kind == KindDeferred })
		if *applyWrites {
			save := func(s *State) error {
				// Remote is authoritative; losing local disk must not lose an intent.
				if store != nil {
					if err := store.Save(s); err != nil {
						return err
					}
				}
				return atomicJSON(*statePath, s)
			}
			if _, err := Apply(c, state, api, entries, save); err != nil {
				return err
			}
		}
		code = 0
		for _, e := range entries {
			if e.Kind.needsReview() || e.Outcome == OutcomeRejected {
				code = 2
			}
		}
		return nil
	}()
	if err != nil {
		code = 1
		if isSyncError(err) {
			rep.Error = err.Error()
		} else {
			// Other errors may quote file contents; report only their type.
			rep.Error = fmt.Sprintf("Invalid input or I/O failure (%T)", err)
		}
	}
	rep.ExitCode = code
	if distinct {
		if err := atomicJSON(*reportPath, rep); err != nil {
			rep.Error = "Could not persist report"
			code, rep.ExitCode = 1, 1
		}
	}
	fmt.Fprintln(stdout, string(canonical(rep, true)))
	return code
}
