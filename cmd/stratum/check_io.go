package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/borovikovd/stratum-agent/check"
	agentv1 "github.com/borovikovd/stratum-agent/gen/stratum/agent/v1"
)

// pullRequest describes the pull request under check: from flags, or from
// the GitHub Actions event.
type pullRequest struct {
	number                                int
	title, author, headBranch, baseBranch string
	headSHA                               string
}

func (p *pullRequest) register(fs *flag.FlagSet) {
	fs.IntVar(&p.number, "pr-number", 0, "pull request number; default: from the GitHub Actions event")
	fs.StringVar(&p.title, "pr-title", "", "pull request title")
	fs.StringVar(&p.author, "pr-author", "", "pull request author's login")
	fs.StringVar(&p.headBranch, "pr-branch", "", "pull request branch")
	fs.StringVar(&p.baseBranch, "pr-base", "", "pull request base branch")
	fs.StringVar(&p.headSHA, "pr-sha", "", "pull request head commit")
}

func (p *pullRequest) resolve() (*agentv1.PullRequest, error) {
	if p.number == 0 {
		if err := p.fromEvent(os.Getenv("GITHUB_EVENT_PATH")); err != nil {
			return nil, err
		}
	}
	return &agentv1.PullRequest{
		Number: int32(p.number), Title: p.title, Author: p.author,
		HeadBranch: p.headBranch, BaseBranch: p.baseBranch, HeadSha: p.headSHA,
	}, nil
}

func (p *pullRequest) fromEvent(path string) error {
	if path == "" {
		return errors.New("set -pr-number, or run on a pull_request event")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the GitHub event: %w", err)
	}
	var event struct {
		PullRequest struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			User   struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(b, &event); err != nil {
		return fmt.Errorf("decode the GitHub event: %w", err)
	}
	pr := event.PullRequest
	if pr.Number == 0 {
		return errors.New("the GitHub event isn't a pull request; run the check on pull_request")
	}
	p.number, p.title, p.author = pr.Number, pr.Title, pr.User.Login
	p.headBranch, p.baseBranch, p.headSHA = pr.Head.Ref, pr.Base.Ref, pr.Head.SHA
	return nil
}

var stepStatuses = map[check.Status]agentv1.StepStatus{
	check.StatusOK:           agentv1.StepStatus_STEP_STATUS_OK,
	check.StatusFailed:       agentv1.StepStatus_STEP_STATUS_FAILED,
	check.StatusSkipped:      agentv1.StepStatus_STEP_STATUS_SKIPPED,
	check.StatusSetupProblem: agentv1.StepStatus_STEP_STATUS_SETUP_PROBLEM,
}

// reportRequest converts a check report for the API, with file paths
// relative to the repository.
func reportRequest(checkID string, r *check.Report, repoRoot string) (*agentv1.ReportCheckRequest, error) {
	req := &agentv1.ReportCheckRequest{
		CheckId:            checkID,
		SetupProblem:       r.SetupProblem,
		AppliedFileChanged: r.AppliedFileChanged,
		DurationMs:         r.Duration.Milliseconds(),
	}
	for _, s := range r.Steps {
		req.Steps = append(req.Steps, &agentv1.Step{Name: s.Name, Detail: s.Detail, Status: stepStatuses[s.Status], DurationMs: s.Duration.Milliseconds()})
	}
	for _, m := range r.Migrations {
		changes, err := m.ChangesJSON()
		if err != nil {
			return nil, err
		}
		source := agentv1.MigrationSource_MIGRATION_SOURCE_BASE
		if m.FromPullRequest {
			source = agentv1.MigrationSource_MIGRATION_SOURCE_PULL_REQUEST
		}
		req.Migrations = append(req.Migrations, &agentv1.Migration{
			Version: m.Version, File: relative(repoRoot, m.Path), Source: source,
			Sql: m.SQL, Applied: m.Applied, Error: m.Error, Changes: changes,
		})
	}
	if r.ResultSchema != nil {
		b, err := json.Marshal(r.ResultSchema)
		if err != nil {
			return nil, err
		}
		req.ResultSchema = b
	}
	return req, nil
}

func relative(root, path string) string {
	absRoot, err1 := filepath.Abs(root)
	absPath, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return path
	}
	if rel, err := filepath.Rel(absRoot, absPath); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}

var conclusions = map[agentv1.Conclusion]string{
	agentv1.Conclusion_CONCLUSION_PASSED:        "passed",
	agentv1.Conclusion_CONCLUSION_WARNINGS:      "passed with warnings",
	agentv1.Conclusion_CONCLUSION_FAILED:        "failed",
	agentv1.Conclusion_CONCLUSION_SETUP_PROBLEM: "setup problem · doesn't block merging",
}

// report prints the verdict for people, writes GitHub annotations and the
// job summary when running in Actions, and returns the exit code.
func report(w io.Writer, v *agentv1.GetCheckVerdictResponse) int {
	var b strings.Builder
	fmt.Fprintf(&b, "Stratum check: %s\n", conclusions[v.GetConclusion()])
	if v.GetSetupProblem() != "" {
		fmt.Fprintf(&b, "  %s\n", v.GetSetupProblem())
	}
	for _, f := range v.GetFindings() {
		fmt.Fprintf(&b, "  %-7s %-22s %s:%d  %s\n", severity(f), f.GetRule(), f.GetFile(), f.GetLine(), f.GetTitle())
	}
	if v.GetDetailsUrl() != "" {
		fmt.Fprintf(&b, "Details: %s\n", v.GetDetailsUrl())
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		annotate(&b, v)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		warn("couldn't print the verdict: %v", err)
	}
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if err := appendFile(path, summary(v)); err != nil {
			warn("couldn't write the job summary: %v", err)
		}
	}
	if v.GetConclusion() == agentv1.Conclusion_CONCLUSION_FAILED {
		return 1
	}
	return 0
}

func severity(f *agentv1.Finding) string {
	if f.GetSeverity() == agentv1.Severity_SEVERITY_ERROR {
		return "error"
	}
	return "warning"
}

// annotate writes workflow commands GitHub shows on the changed lines.
// GitHub shows only the first ten of each level per step; the job summary
// has the full list.
func annotate(w *strings.Builder, v *agentv1.GetCheckVerdictResponse) {
	if v.GetSetupProblem() != "" {
		fmt.Fprintf(w, "::warning title=Stratum setup problem::%s\n", escape(v.GetSetupProblem()))
	}
	for _, f := range v.GetFindings() {
		fmt.Fprintf(w, "::%s file=%s,line=%d,title=%s::%s\n", severity(f), f.GetFile(), max(f.GetLine(), 1), escape(f.GetTitle()), escape(f.GetDetail()+" "+f.GetFix()))
	}
}

func summary(v *agentv1.GetCheckVerdictResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Stratum: %s\n\n", conclusions[v.GetConclusion()])
	if v.GetSetupProblem() != "" {
		fmt.Fprintf(&b, "%s\n\n", v.GetSetupProblem())
	}
	for _, f := range v.GetFindings() {
		fmt.Fprintf(&b, "### %s: %s\n\n`%s:%d` · `%s`\n\n%s\n\n**Suggested fix.** %s\n\n", severity(f), f.GetTitle(), f.GetFile(), f.GetLine(), f.GetRule(), f.GetDetail(), f.GetFix())
		if f.GetFixSql() != "" {
			fmt.Fprintf(&b, "```sql\n%s\n```\n\n", f.GetFixSql())
		}
	}
	if len(v.GetFindings()) == 0 && v.GetSetupProblem() == "" {
		b.WriteString("No findings.\n\n")
	}
	if v.GetDetailsUrl() != "" {
		fmt.Fprintf(&b, "[Open in Stratum](%s)\n", v.GetDetailsUrl())
	}
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

func appendFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return errors.Join(err, f.Close())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string) int64 {
	n, _ := strconv.ParseInt(os.Getenv(key), 10, 64)
	return n
}
