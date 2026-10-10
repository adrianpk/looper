package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type pullRequest struct {
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	HTMLURL      string    `json:"html_url"`
	State        string    `json:"state"`
	Merged       bool      `json:"merged"`
	Mergeable    bool      `json:"mergeable"`
	Draft        bool      `json:"draft"`
	MergedCommit string    `json:"merge_commit_sha"`
	Base         reference `json:"base"`
	Head         reference `json:"head"`
}

type reference struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repo"`
}

// Tea owns authentication. Tokens never enter configuration, arguments or checkpoints.
func (a *app) api(method, endpoint string, body any, result any) error {
	repository := a.config.Repository
	if repository == "" {
		origin, err := git(a.config.Repo, "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		repository, err = repositoryFromOrigin(origin)
		if err != nil {
			return err
		}
	}
	args := []string{"api", "--repo", repository, "--remote", "origin", "--method", method}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		args = append(args, "--data", string(b))
	}
	endpoint = strings.ReplaceAll(endpoint, "{owner}/{repo}", repository)
	args = append(args, endpoint)
	var out string
	var err error
	if method == "GET" {
		out, err = accessCommand("forge", a.config.Repo, "tea", args...)
	} else {
		out, err = command(a.config.Repo, "tea", args...)
		if err != nil {
			err = classifyAccess("forge", err, 1)
		}
	}
	if err != nil {
		return err
	}
	if result != nil {
		if err := json.Unmarshal([]byte(out), result); err != nil {
			return fmt.Errorf("Forgejo returned a non-JSON response for %s", method)
		}
	}
	return nil
}

func repositoryFromOrigin(raw string) (string, error) {
	var path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		path = u.Path
	} else if i := strings.Index(raw, ":"); i >= 0 {
		path = raw[i+1:]
	} else {
		return "", errors.New("origin must be a Forgejo SSH or HTTPS URL")
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("cannot resolve owner/repository from origin")
	}
	return path, nil
}

func (a *app) endpoint(suffix string) string { return "repos/" + a.config.Repository + suffix }

func (a *app) pull(n int) (pullRequest, error) {
	var p pullRequest
	err := a.api("GET", a.endpoint("/pulls/"+strconv.Itoa(n)), nil, &p)
	return p, err
}

func (a *app) discover(s slice) (*pullRequest, error) {
	var found *pullRequest
	for page := 1; page <= 100; page++ {
		q := url.Values{"state": {"all"}, "base": {"dev"}, "head": {s.Branch}, "limit": {"50"}, "page": {strconv.Itoa(page)}}
		var prs []pullRequest
		if err := a.api("GET", a.endpoint("/pulls?")+q.Encode(), nil, &prs); err != nil {
			return nil, err
		}
		for _, p := range prs {
			if p.Head.Ref == s.Branch && p.Head.Repo.FullName == a.config.Repository && p.Base.Ref == "dev" {
				if found != nil {
					return nil, fmt.Errorf("multiple PRs found for branch %s", s.Branch)
				}
				copy := p
				found = &copy
			}
		}
		if len(prs) < 50 {
			return found, nil
		}
	}
	return nil, errors.New("PR discovery exceeds 5000 results")
}

type parsedBody struct {
	Commands     []string
	ReportCommit string
}

func parseBody(body string, s slice, forge, repo string) (parsedBody, error) {
	var result parsedBody
	// A single final LF is the normal text-file terminator, not an extra section.
	body = strings.TrimSuffix(body, "\n")
	if len(body) > 128<<10 || strings.Contains(body, "\r") || body != strings.TrimSpace(body) {
		return result, errors.New("body has trailing/leading whitespace, CRLF or excessive size")
	}
	lines := strings.Split(body, "\n")
	sections := []string{"## Summary", "## Changes", "## Validation"}
	positions := []int{}
	for i, line := range lines {
		if line != strings.TrimRight(line, " \t") {
			return result, errors.New("body contains trailing whitespace")
		}
		if strings.HasPrefix(line, "#") {
			if len(positions) >= 3 || line != sections[len(positions)] {
				return result, errors.New("body headings must be exactly Summary, Changes, Validation")
			}
			positions = append(positions, i)
		}
	}
	if len(positions) != 3 || positions[0] != 0 {
		return result, errors.New("body requires the three canonical headings")
	}
	for _, p := range positions {
		if p+1 >= len(lines) || lines[p+1] != "" {
			return result, errors.New("headings require one following blank line")
		}
	}
	if positions[1] < 4 || positions[2]-positions[1] < 5 || lines[positions[1]-1] != "" || lines[positions[2]-1] != "" {
		return result, errors.New("sections require canonical blank-line separation")
	}
	summary := lines[2 : positions[1]-1]
	for _, line := range summary {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "```") || line[0] == ' ' || line[0] == '\t' {
			return result, errors.New("Summary must be one prose paragraph")
		}
	}
	changes := lines[positions[1]+2 : positions[2]-1]
	if len(changes) < 2 {
		return result, errors.New("Changes requires a delivered outcome and final report link")
	}
	for _, line := range changes {
		if !strings.HasPrefix(line, "- ") || strings.TrimSpace(line[2:]) == "" {
			return result, errors.New("Changes must contain only nonempty, single-line '- ' items")
		}
	}
	pattern := `^- \[Slice ` + strconv.Itoa(s.Number) + ` report\]\(` + regexp.QuoteMeta(forge+"/"+repo+"/src/commit/") + `([0-9a-f]{40})/` + regexp.QuoteMeta(s.Report) + `\)$`
	m := regexp.MustCompile(pattern).FindStringSubmatch(changes[len(changes)-1])
	if m == nil {
		return result, errors.New("last Changes item must link the canonical report at its full immutable introduction commit")
	}
	result.ReportCommit = m[1]
	seen := map[string]bool{}
	for _, line := range lines[positions[2]+2:] {
		if !strings.HasPrefix(line, "- `") || !strings.HasSuffix(line, "`") || len(line) < 5 {
			return result, errors.New("Validation must contain only exact commands in single-backtick list items")
		}
		cmd := line[3 : len(line)-1]
		if strings.TrimSpace(cmd) != cmd || cmd == "" || strings.Contains(cmd, "`") || seen[cmd] {
			return result, errors.New("Validation commands must be nonempty and unique")
		}
		seen[cmd] = true
		result.Commands = append(result.Commands, cmd)
	}
	if len(result.Commands) == 0 || result.Commands[len(result.Commands)-1] != "git diff --check" {
		return result, errors.New("git diff --check must be the final Validation command")
	}
	if len(s.RequiredCommands) > 0 {
		if len(result.Commands) < len(s.RequiredCommands) {
			return result, errors.New("required validation commands missing")
		}
		for i, cmd := range s.RequiredCommands {
			if result.Commands[i] != cmd {
				return result, errors.New("required validation command order differs from configuration")
			}
		}
	}
	return result, nil
}

func (a *app) checkPR(s slice, p pullRequest) (parsedBody, error) {
	if p.Title != s.Title {
		return parsedBody{}, errors.New("PR title differs from the exact approved title")
	}
	if p.Draft || p.Head.Ref != s.Branch || p.Base.Ref != "dev" || p.Head.Repo.FullName != a.config.Repository || p.Base.Repo.FullName != a.config.Repository || p.Base.Repo.HTMLURL != a.config.Forge+"/"+a.config.Repository || !fullSHA.MatchString(p.Head.SHA) || !fullSHA.MatchString(p.Base.SHA) {
		return parsedBody{}, errors.New("PR repository, branch, dev base, draft state or full SHA mismatch")
	}
	if p.HTMLURL != fmt.Sprintf("%s/%s/pulls/%d", a.config.Forge, a.config.Repository, p.Number) {
		return parsedBody{}, errors.New("PR URL is not canonical")
	}
	parsed, err := parseBody(p.Body, s, a.config.Forge, a.config.Repository)
	if err != nil {
		return parsed, err
	}
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", parsed.ReportCommit, p.Head.SHA); err != nil {
		return parsed, errors.New("report introduction is not an ancestor of the PR head")
	}
	intro, err := git(a.config.Repo, "log", p.Head.SHA, "--diff-filter=A", "--format=%H", "--", s.Report)
	if err != nil {
		return parsed, err
	}
	if !strings.Contains("\n"+intro+"\n", "\n"+parsed.ReportCommit+"\n") {
		return parsed, errors.New("report URL commit did not introduce the report")
	}
	return parsed, nil
}
