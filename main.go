package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const version = "0.4.3"

func main() {
	if err := execute(os.Args[1:]); err != nil {
		os.Exit(reportError(err))
	}
}

func execute(args []string) error {
	if len(args) != 0 && args[0] == "--watch" {
		args = append([]string{"watch"}, args[1:]...)
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return printHelp()
	}
	if args[0] == "version" {
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			return printHelp()
		}
		fmt.Println("looper", version)
		return nil
	}
	if agentInvocation(args) {
		if args[0] == "run" {
			args = append([]string{"resume"}, args[1:]...)
		}
		return executeAgent(args)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.Usage = func() {}
	configPath := fs.String("config", "looper.json", "local configuration file")
	switch args[0] {
	case "init":
		err := initConfig(fs, args[1:])
		if errors.Is(err, flag.ErrHelp) {
			return printHelp()
		}
		return err
	case "run", "status", "check", "validate", "recover", "activity", "preflight", "authorize-gate-repairs", "authorize-additional-gate-repairs", "retry-gate":
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	index := fs.Int("slice", 0, "slice number")
	pr := fs.Int("pr", 0, "pull request number")
	worktree := fs.String("worktree", "", "validation checkout")
	retryUnmerged := fs.Bool("retry-unmerged", false, "operator confirmed the previous uncertain merge did not apply")
	gatePolicy := fs.String("policy", "", "explicitly approved final-gate repair policy JSON")
	failedHead := fs.String("failed-head", "", "exact retained failed gate candidate")
	retryReason := fs.String("reason", "", "operator-confirmed environmental correction for one gate retry")
	showActivity := fs.Bool("activity", false, "show live worker activity; visibility can change during a run")
	color := fs.String("color", string(defaultColor), "terminal colors: auto, always, never")
	output := fs.String("output", string(defaultOutput), "stream output: jsonl, human")
	activityOn := fs.Bool("on", false, "enable activity for the running controller")
	activityOff := fs.Bool("off", false, "hide activity for the running controller")
	activityToggle := fs.Bool("toggle", false, "toggle activity for the running controller")
	var commands stringList
	fs.Var(&commands, "command", "exact validation command; repeat to preserve order")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return printHelp()
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	repairAuthorization := args[0] == "authorize-gate-repairs" || args[0] == "authorize-additional-gate-repairs"
	if *gatePolicy != "" && !repairAuthorization || *failedHead != "" && !repairAuthorization && args[0] != "retry-gate" {
		return errors.New("--policy belongs to repair authorization; --failed-head belongs to repair authorization or retry-gate")
	}
	if *retryReason != "" && args[0] != "retry-gate" {
		return errors.New("--reason belongs to retry-gate")
	}
	if args[0] == "retry-gate" {
		if err := authorizeGateRetry(*configPath, *failedHead, *retryReason); err != nil {
			return err
		}
		fmt.Println("One same-candidate gate retry authorized. Failure evidence, configuration, repair history and worker budgets preserved. Resume the original run to execute it.")
		return nil
	}
	if repairAuthorization {
		if *gatePolicy == "" {
			return fmt.Errorf("%s requires --policy", args[0])
		}
		authorize := authorizeGateRepairs
		if args[0] == "authorize-additional-gate-repairs" {
			if *failedHead == "" {
				return errors.New("authorize-additional-gate-repairs requires --failed-head")
			}
			authorize = authorizeAdditionalGateRepairs
		}
		if err := authorize(*configPath, *gatePolicy, *failedHead); err != nil {
			return err
		}
		fmt.Println("Final-gate repair authorization recorded. Existing authority, configuration, progress and budgets preserved. No worker started.")
		return nil
	}
	if !validColor(colorMode(*color)) {
		return errors.New("--color must be auto, always, or never")
	}
	outputChosen := false
	fs.Visit(func(f *flag.Flag) { outputChosen = outputChosen || f.Name == "output" })
	if !validOutput(outputFormat(*output)) || outputChosen && args[0] != "run" {
		return errors.New("--output jsonl|human belongs to run or resume")
	}
	if *showActivity && args[0] != "run" {
		return errors.New("--activity belongs to run")
	}
	if (*activityOn || *activityOff || *activityToggle) && args[0] != "activity" {
		return errors.New("--on, --off and --toggle belong to activity")
	}
	if args[0] == "activity" {
		colorChosen := false
		fs.Visit(func(f *flag.Flag) { colorChosen = colorChosen || f.Name == "color" })
		mode := "status"
		count := 0
		for _, choice := range []struct {
			enabled bool
			mode    string
		}{{*activityOn, "on"}, {*activityOff, "off"}, {*activityToggle, "toggle"}} {
			if choice.enabled {
				mode = choice.mode
				count++
			}
		}
		if count > 1 {
			return errors.New("select only one of --on, --off and --toggle")
		}
		if colorChosen {
			result, err := activityRequest(*configPath, "color:"+*color)
			if err != nil {
				return err
			}
			newOutput(os.Stdout, colorMode(*color)).printf(activityRunning, "%s\n", result)
		}
		result, err := activityRequest(*configPath, mode)
		if err != nil {
			return err
		}
		fmt.Println(result)
		return nil
	}
	a, err := loadApp(*configPath)
	if err != nil {
		return err
	}
	a.output = newOutput(os.Stdout, colorMode(*color))
	switch args[0] {
	case "preflight":
		if err := a.preflight(); err != nil {
			return err
		}
		fmt.Println("Execution prerequisites verified in this process context.")
		return nil
	case "status":
		return printJSON(a.state)
	case "check":
		if *index < 1 || *index > len(a.config.Slices) || *pr < 1 {
			return errors.New("check requires a valid --slice and --pr")
		}
		p, err := a.pull(*pr)
		if err != nil {
			return err
		}
		parsed, err := a.checkPR(a.config.Slices[*index-1], p)
		if err != nil {
			return err
		}
		a.printf(activitySuccess, "PR #%d: canonical title/body, branch, base, report introduction and head verified; %d validation commands\n", p.Number, len(parsed.Commands))
		return nil
	case "validate":
		if *worktree == "" || len(commands) == 0 {
			return errors.New("validate requires --worktree and at least one --command")
		}
		return a.validate(*worktree, commands)
	case "run":
		return runController(a, controllerOptions{Activity: *showActivity, Color: colorMode(*color), Output: outputFormat(*output)})
	case "recover":
		if !*retryUnmerged || a.state.Pending == nil {
			return errors.New("recover requires a pending merge and explicit --retry-unmerged")
		}
		lock, err := os.OpenFile(filepath.Join(a.dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer func() { _ = lock.Close() }()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errors.New("controller is still running")
		}
		p, err := a.pull(a.state.Pending.PR)
		if err != nil {
			return err
		}
		if p.Merged {
			return a.reconcile()
		}
		if p.State != "open" || p.Head.SHA != a.state.Pending.Head {
			return errors.New("remote state differs from the uncertain submission; manual inspection required")
		}
		a.state.Pending = nil
		a.state.Status = "ready"
		a.state.Feedback = "Operator confirmed the previous uncertain merge did not apply; repeat canonical checks before a new submission"
		return a.save()
	}
	return nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	d := json.NewDecoder(io.LimitReader(f, 2<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON data")
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".looper-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// capture bounds diagnostic output without stopping the child process.
type capture struct {
	b         bytes.Buffer
	limit     int
	truncated bool
}

func (c *capture) Write(p []byte) (int, error) {
	n := len(p)
	left := c.limit - c.b.Len()
	if left > 0 {
		_, _ = c.b.Write(p[:min(left, n)])
	}
	c.truncated = c.truncated || n > left
	return n, nil
}

func command(dir, name string, args ...string) (string, error) {
	c, err := executionCommand(context.Background(), dir, name, args...)
	if err != nil {
		return "", err
	}
	return commandOutput(c)
}

func commandOutput(c *exec.Cmd) (string, error) {
	var out, diagnostic capture
	out.limit, diagnostic.limit = 8<<20, 4096
	c.Stdout, c.Stderr = &out, &diagnostic
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s failed: %w: %s", filepath.Base(c.Path), err, strings.TrimSpace(diagnostic.b.String()))
	}
	if out.truncated {
		return "", fmt.Errorf("%s output exceeds 8 MiB", filepath.Base(c.Path))
	}
	return strings.TrimSpace(out.b.String()), nil
}

func git(dir string, args ...string) (string, error) { return command(dir, "git", args...) }

func cleanHead(dir string) (string, error) {
	status, err := git(dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", errors.New("checkout must be clean, including untracked files")
	}
	return git(dir, "rev-parse", "HEAD")
}
