package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type slice struct {
	Number           int      `json:"number"`
	Branch           string   `json:"branch"`
	Title            string   `json:"title"`
	Report           string   `json:"report"`
	RequiredCommands []string `json:"required_commands,omitempty"`
}

type config struct {
	Version      int      `json:"version"`
	Repo         string   `json:"repo"`
	Origin       string   `json:"origin"`
	Forge        string   `json:"forge"`
	Repository   string   `json:"repository"`
	Plan         string   `json:"plan"`
	Tracker      string   `json:"tracker"`
	Session      string   `json:"session,omitempty"`
	FullGate     string   `json:"full_gate"`
	MaxStalled   int      `json:"max_stalled_turns"`
	MaxTurns     int      `json:"max_worker_turns"`
	Slices       []slice  `json:"slices"`
	ClosurePaths []string `json:"closure_paths"`
}

type evidence struct {
	Head     string   `json:"head"`
	Commands []string `json:"commands"`
	Log      string   `json:"log"`
}

type mergeRecord struct {
	PR           int    `json:"pr"`
	Head         string `json:"head"`
	MergedCommit string `json:"merged_commit"`
	Recorded     bool   `json:"activity_recorded"`
}

type pendingMerge struct {
	PR   int    `json:"pr"`
	Head string `json:"head"`
}

type state struct {
	ConfigHash  string        `json:"config_hash"`
	Status      string        `json:"status"`
	Next        int           `json:"next_slice_index"`
	Session     string        `json:"session,omitempty"`
	Turns       int           `json:"worker_turns"`
	Stalled     int           `json:"stalled_turns"`
	Fingerprint string        `json:"last_fingerprint,omitempty"`
	Feedback    string        `json:"feedback,omitempty"`
	Pending     *pendingMerge `json:"pending_merge,omitempty"`
	Merged      []mergeRecord `json:"merged"`
	Gate        *evidence     `json:"gate,omitempty"`
	Blocker     *runBlocker   `json:"blocker,omitempty"`
}

type app struct {
	config   config
	state    state
	path     string
	dir      string
	reply    workerResult
	activity *activityMonitor
	runtime  *runRuntime
	output   *terminalOutput
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func loadApp(path string) (*app, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return nil, err
	}
	a := &app{path: p, dir: p + ".state"}
	if err := readJSON(p, &a.config); err != nil {
		return nil, err
	}
	c := a.config
	if c.Version != 1 || c.Repo == "" || c.Forge == "" || c.Repository == "" || c.FullGate == "" || c.MaxStalled < 1 || c.MaxTurns < 1 || len(c.Slices) == 0 {
		return nil, errors.New("incomplete or unsupported configuration")
	}
	for _, path := range append([]string{c.Plan, c.Tracker}, c.ClosurePaths...) {
		if !safePath(path) {
			return nil, fmt.Errorf("unsafe repository path %q", path)
		}
	}
	for i, s := range c.Slices {
		if s.Number != i+1 || s.Branch == "" || s.Branch == "dev" || s.Branch == "main" || strings.ContainsAny(s.Branch, " \n\r") || !safePath(s.Report) || s.Title == "" {
			return nil, errors.New("invalid ordered slice identity")
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	a.state = state{ConfigHash: digest(b), Session: c.Session, Status: "ready"}
	if err := readJSON(filepath.Join(a.dir, "state.json"), &a.state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if a.state.ConfigHash != digest(b) {
		return nil, errors.New("configuration changed after checkpoint creation; use a new configuration path")
	}
	if a.state.Next < 0 || a.state.Next > len(c.Slices) || len(a.state.Merged) != a.state.Next {
		return nil, errors.New("invalid checkpoint slice position")
	}
	return a, nil
}

func (a *app) save() error { return writeJSON(filepath.Join(a.dir, "state.json"), a.state) }

func safePath(p string) bool {
	return p != "" && !filepath.IsAbs(p) && filepath.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.ContainsAny(p, "\r\n")
}

func relativePath(repo, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo, p)
	}
	r, err := filepath.Rel(repo, p)
	if err != nil || !safePath(r) {
		return "", errors.New("plan and tracker must be inside the repository")
	}
	return r, nil
}

func initConfig(fs *flag.FlagSet, args []string) error {
	repo := fs.String("repo", "", "primary repository checkout")
	plan := fs.String("plan", "", "approved plan path")
	tracker := fs.String("tracker", "", "approved tracker path")
	gate := fs.String("gate", "", "exact full-gate shell command from the approved plan")
	session := fs.String("session", "", "optional inactive Codex conversation UUID")
	out := fs.String("out", "looper.json", "local output path outside the repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" || *plan == "" || *tracker == "" || strings.TrimSpace(*gate) == "" || fs.NArg() != 0 {
		return errors.New("init requires --repo, --plan, --tracker and --gate")
	}
	r, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	r, err = filepath.EvalSymlinks(r)
	if err != nil {
		return err
	}
	root, err := git(r, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if root != r {
		return errors.New("--repo must name the checkout root")
	}
	p, err := relativePath(r, *plan)
	if err != nil {
		return err
	}
	t, err := relativePath(r, *tracker)
	if err != nil {
		return err
	}
	if _, err = cleanHead(r); err != nil {
		return err
	}
	branch, err := git(r, "branch", "--show-current")
	if err != nil {
		return err
	}
	if branch != "dev" {
		return errors.New("init requires the primary dev checkout")
	}
	for _, path := range []string{p, t} {
		if _, err := git(r, "ls-files", "--error-unmatch", path); err != nil {
			return errors.New("plan and tracker must already be committed")
		}
	}
	pb, err := os.ReadFile(filepath.Join(r, p))
	if err != nil {
		return err
	}
	tb, err := os.ReadFile(filepath.Join(r, t))
	if err != nil {
		return err
	}
	if !hasApproval(string(pb)) || !hasApproval(string(tb)) {
		return errors.New("plan and tracker require explicit Approved metadata")
	}
	slices, err := parseSliceTable(string(pb))
	if err != nil {
		return err
	}
	other, err := parseSliceTable(string(tb))
	if err != nil {
		return err
	}
	if !sameSlices(slices, other) {
		return errors.New("plan and tracker slice identities disagree")
	}
	origin, err := git(r, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	a := &app{config: config{Repo: r}}
	var metadata struct {
		FullName    string `json:"full_name"`
		HTMLURL     string `json:"html_url"`
		AllowRebase bool   `json:"allow_rebase"`
	}
	if err := a.api("GET", "repos/{owner}/{repo}", nil, &metadata); err != nil {
		return err
	}
	if !metadata.AllowRebase {
		return errors.New("canonical repository does not allow Rebase + Fast-forward")
	}
	suffix := "/" + metadata.FullName
	if !strings.HasPrefix(metadata.HTMLURL, "https://") || !strings.HasSuffix(metadata.HTMLURL, suffix) {
		return errors.New("invalid canonical Forgejo repository URL")
	}
	c := config{Version: 1, Repo: r, Origin: origin, Forge: strings.TrimSuffix(metadata.HTMLURL, suffix), Repository: metadata.FullName, Plan: p, Tracker: t, FullGate: *gate, Session: *session, MaxStalled: 5, MaxTurns: 200, Slices: slices, ClosurePaths: []string{p, t}}
	for _, s := range slices {
		c.ClosurePaths = append(c.ClosurePaths, s.Report)
	}
	link := regexp.MustCompile(`(?m)^(?:Concern|Model|Spec|Specification):.*\]\(([^)]+)\)`)
	for _, doc := range []struct {
		path string
		body []byte
	}{{p, pb}, {t, tb}} {
		for _, m := range link.FindAllStringSubmatch(string(doc.body), -1) {
			path := filepath.Clean(filepath.Join(filepath.Dir(doc.path), m[1]))
			if safePath(path) {
				c.ClosurePaths = append(c.ClosurePaths, path)
			}
		}
	}
	output, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	inside, err := filepath.Rel(r, output)
	if err != nil {
		return err
	}
	if safePath(inside) {
		return errors.New("configuration must be outside the product repository")
	}
	if _, err := os.Stat(output); err == nil {
		return errors.New("configuration already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeJSON(output, c); err != nil {
		return err
	}
	created, err := loadApp(output)
	if err != nil {
		return err
	}
	if _, err := registerRun(created); err != nil {
		return fmt.Errorf("configuration created at %s, but registration failed: %w", output, err)
	}
	fmt.Printf("Created %s for %d slices. No Codex execution or merge started.\n", output, len(slices))
	return nil
}

func hasApproval(doc string) bool {
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "Approved:") && strings.TrimSpace(strings.TrimPrefix(line, "Approved:")) != "" {
			return true
		}
	}
	return false
}

func parseSliceTable(doc string) ([]slice, error) {
	var columns map[string]int
	var result []slice
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		parts := strings.Split(strings.Trim(line, "| \r"), "|")
		for i := range parts {
			parts[i] = strings.Trim(strings.TrimSpace(parts[i]), "`")
		}
		if len(parts) > 0 && parts[0] == "Slice" {
			columns = map[string]int{}
			for i, p := range parts {
				columns[p] = i
			}
			continue
		}
		if len(parts) == 0 || !strings.HasPrefix(parts[0], "Slice ") {
			continue
		}
		get := func(names ...string) string {
			for _, name := range names {
				if i, ok := columns[name]; ok && i < len(parts) {
					return parts[i]
				}
			}
			return ""
		}
		n, err := strconv.Atoi(strings.TrimPrefix(parts[0], "Slice "))
		if err != nil || n != len(result)+1 {
			return nil, errors.New("slice table must have consecutive numbers starting at 1")
		}
		s := slice{Number: n, Branch: get("Exact branch", "Branch"), Title: get("Expected PR title"), Report: get("Report")}
		if s.Branch == "" || s.Title == "" || !safePath(s.Report) {
			return nil, fmt.Errorf("Slice %d lacks canonical branch, PR title or report", n)
		}
		result = append(result, s)
	}
	if len(result) == 0 || len(result) > 100 {
		return nil, errors.New("expected between 1 and 100 slices")
	}
	return result, nil
}

func sameSlices(a, b []slice) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Number != b[i].Number || a[i].Branch != b[i].Branch || a[i].Title != b[i].Title || a[i].Report != b[i].Report {
			return false
		}
	}
	return true
}

func (a *app) verifyIdentity() error {
	origin, err := git(a.config.Repo, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if origin != a.config.Origin {
		return errors.New("origin URL changed")
	}
	for _, path := range []string{a.config.Plan, a.config.Tracker} {
		doc, err := git(a.config.Repo, "show", "origin/dev:"+path)
		if err != nil {
			return err
		}
		if !hasApproval(doc) {
			return errors.New("canonical plan/tracker no longer approved")
		}
		s, err := parseSliceTable(doc)
		if err != nil {
			return err
		}
		if !sameSlices(s, a.config.Slices) {
			return errors.New("canonical slice map changed; explicit reconfiguration is required")
		}
	}
	return nil
}
