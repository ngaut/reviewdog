package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/oauth2"

	"github.com/google/go-github/github"
	"github.com/haya14busa/errorformat"
	"github.com/haya14busa/errorformat/fmts"
	"github.com/haya14busa/reviewdog"
	"github.com/mattn/go-shellwords"
)

const usageMessage = "" +
	`Usage:	reviewdog [flags]
	reviewdog accepts any compiler or linter results from stdin and filters
	them by diff for review. reviewdog also can posts the results as a comment to
	GitHub if you use reviewdog in CI service.
`

// flags
var (
	diffCmd    string
	diffCmdDoc = `diff command (e.g. "git diff"). diff flag is ignored if you pass "ci" flag`

	diffStrip int

	efms strslice
	f    string // errorformat name
	list bool   // list supported errorformat name

	ci    string
	ciDoc = `CI service (supported travis, circle-ci, droneio(OSS 0.4), common)
	If you use "ci" flag, you need to set REVIEWDOG_GITHUB_API_TOKEN environment
	variable.  Go to https://github.com/settings/tokens and create new Personal
	access token with repo scope.

	"common" requires following environment variables
		CI_PULL_REQUEST	Pull Request number (e.g. 14)
		CI_COMMIT	SHA1 for the current build
		CI_REPO_OWNER	repository owner (e.g. "haya14busa" for https://github.com/haya14busa/reviewdog)
		CI_REPO_NAME	repository name (e.g. "reviewdog" for https://github.com/haya14busa/reviewdog)
`
)

func init() {
	flag.StringVar(&diffCmd, "diff", "", diffCmdDoc)
	flag.IntVar(&diffStrip, "strip", 1, "strip NUM leading components from diff file names (equivalent to `patch -p`) (default is 1 for git diff)")
	flag.Var(&efms, "efm", "list of errorformat (https://github.com/haya14busa/errorformat)")
	flag.StringVar(&f, "f", "", "errorformat name (run -list to see supported errorformat name)")
	flag.BoolVar(&list, "list", false, "list available errorformat names as -f arg")
	flag.StringVar(&ci, "ci", "", ciDoc)
}

func usage() {
	fmt.Fprintln(os.Stderr, usageMessage)
	fmt.Fprintln(os.Stderr, "Flags:")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if err := run(os.Stdin, os.Stdout, diffCmd, diffStrip, efms, f, list, ci); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(r io.Reader, w io.Writer, diffCmd string, diffStrip int, efms []string, f string, list bool, ci string) error {
	if list {
		return runList(w)
	}

	// use defined errorformat
	if f != "" {
		if len(efms) > 0 {
			return errors.New("you cannot specify both -f and -efms at the same time")
		}
		efm, ok := fmts.DefinedFmts()[f]
		if !ok {
			return fmt.Errorf("%q is not supported. Use -efms or consider to add new errrorformat to https://github.com/haya14busa/errorformat", f)
		}
		efms = efm.Errorformat
	}

	p, err := efmParser(efms)
	if err != nil {
		return err
	}

	var cs reviewdog.CommentService
	var ds reviewdog.DiffService

	if ci != "" {
		if os.Getenv("REVIEWDOG_GITHUB_API_TOKEN") != "" {
			gs, isPR, err := githubService(ci)
			if err != nil {
				return err
			}
			if !isPR {
				fmt.Fprintf(os.Stderr, "this is not PullRequest build. CI: %v\n", ci)
				return nil
			}
			cs = gs
			ds = gs
		} else {
			fmt.Fprintf(os.Stderr, "REVIEWDOG_GITHUB_API_TOKEN is not set\n")
			return nil
		}
	} else {
		// local
		cs = reviewdog.NewCommentWriter(w)
		d, err := diffService(diffCmd, diffStrip)
		if err != nil {
			return err
		}
		ds = d
	}

	app := reviewdog.NewReviewdog(p, cs, ds)
	if err := app.Run(r); err != nil {
		return err
	}
	if fcs, ok := cs.(FlashCommentService); ok {
		// Output log to writer
		for _, c := range fcs.ListPostComments() {
			fmt.Fprintln(w, strings.Join(c.Lines, "\n"))
		}
		return fcs.Flash()
	}
	return nil
}

func runList(w io.Writer) error {
	tabw := tabwriter.NewWriter(w, 0, 8, 0, '\t', 0)
	for _, f := range sortedFmts(fmts.DefinedFmts()) {
		fmt.Fprintf(tabw, "%s\t%s\t- %s\n", f.Name, f.Description, f.URL)
	}
	return tabw.Flush()
}

type byFmtName []*fmts.Fmt

func (p byFmtName) Len() int           { return len(p) }
func (p byFmtName) Less(i, j int) bool { return p[i].Name < p[j].Name }
func (p byFmtName) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func sortedFmts(fs fmts.Fmts) []*fmts.Fmt {
	r := make([]*fmts.Fmt, 0, len(fs))
	for _, f := range fs {
		r = append(r, f)
	}
	sort.Sort(byFmtName(r))
	return r
}

// FlashCommentService is CommentService which uses Flash method to post comment.
type FlashCommentService interface {
	reviewdog.CommentService
	ListPostComments() []*reviewdog.Comment
	Flash() error
}

func efmParser(efms []string) (reviewdog.Parser, error) {
	efm, err := errorformat.NewErrorformat(efms)
	if err != nil {
		return nil, err
	}
	return reviewdog.NewErrorformatParser(efm), nil
}

func diffService(s string, strip int) (reviewdog.DiffService, error) {
	cmds, err := shellwords.Parse(s)
	if err != nil {
		return nil, err
	}
	if len(cmds) < 1 {
		return nil, errors.New("diff command is empty")
	}
	cmd := exec.Command(cmds[0], cmds[1:]...)
	d := reviewdog.NewDiffCmd(cmd, strip)
	return d, nil
}

func githubService(ci string) (githubservice *reviewdog.GitHubPullRequest, isPR bool, err error) {
	token, err := nonEmptyEnv("REVIEWDOG_GITHUB_API_TOKEN")
	if err != nil {
		return nil, false, err
	}
	var g *GitHubPR
	switch ci {
	case "travis":
		g, isPR, err = travis()
	case "circle-ci":
		g, isPR, err = circleci()
	case "droneio":
		g, isPR, err = droneio()
	case "common":
		g, isPR, err = commonci()
	default:
		return nil, false, fmt.Errorf("unsupported CI: %v", ci)
	}
	if err != nil {
		return nil, false, err
	}
	// TODO: support commit build
	if !isPR {
		return nil, false, nil
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(oauth2.NoContext, ts)
	client := github.NewClient(tc)
	githubservice = reviewdog.NewGitHubPullReqest(client, g.owner, g.repo, g.pr, g.sha)
	return githubservice, true, nil
}

func travis() (g *GitHubPR, isPR bool, err error) {
	prs := os.Getenv("TRAVIS_PULL_REQUEST")
	if prs == "false" {
		return nil, false, nil
	}
	pr, err := strconv.Atoi(prs)
	if err != nil {
		return nil, true, fmt.Errorf("unexpected env variable. TRAVIS_PULL_REQUEST=%v", prs)
	}
	reposlug, err := nonEmptyEnv("TRAVIS_REPO_SLUG")
	if err != nil {
		return nil, true, err
	}
	rss := strings.SplitN(reposlug, "/", 2)
	if len(rss) < 2 {
		return nil, true, fmt.Errorf("unexpected env variable. TRAVIS_REPO_SLUG=%v", reposlug)
	}
	owner, repo := rss[0], rss[1]

	sha, err := nonEmptyEnv("TRAVIS_PULL_REQUEST_SHA")
	if err != nil {
		return nil, true, err
	}

	g = &GitHubPR{
		owner: owner,
		repo:  repo,
		pr:    pr,
		sha:   sha,
	}
	return g, true, nil
}

// https://circleci.com/docs/environment-variables/
func circleci() (g *GitHubPR, isPR bool, err error) {
	var prs string // pull request number in string
	// For Pull Request from a same repository
	// e.g. https: //github.com/haya14busa/reviewdog/pull/6
	// it might be better to support CI_PULL_REQUESTS instead.
	prs = os.Getenv("CI_PULL_REQUEST")
	if prs == "" {
		// For Pull Request by a fork repository
		// e.g. 6
		prs = os.Getenv("CIRCLE_PR_NUMBER")
	}
	if prs == "" {
		// not a pull-request build
		return nil, false, nil
	}
	// regexp.MustCompile() in func intentionally because this func is called
	// once for one run.
	re := regexp.MustCompile(`[1-9]\d*$`)
	prm := re.FindString(prs)
	pr, err := strconv.Atoi(prm)
	if err != nil {
		return nil, true, fmt.Errorf("unexpected env variable (CI_PULL_REQUEST or CIRCLE_PR_NUMBER): %v", prs)
	}
	owner, err := nonEmptyEnv("CIRCLE_PROJECT_USERNAME")
	if err != nil {
		return nil, true, err
	}
	repo, err := nonEmptyEnv("CIRCLE_PROJECT_REPONAME")
	if err != nil {
		return nil, true, err
	}
	sha, err := nonEmptyEnv("CIRCLE_SHA1")
	if err != nil {
		return nil, true, err
	}
	g = &GitHubPR{
		owner: owner,
		repo:  repo,
		pr:    pr,
		sha:   sha,
	}
	return g, true, nil
}

// http://readme.drone.io/usage/variables/
func droneio() (g *GitHubPR, isPR bool, err error) {
	var prs string // pull request number in string
	prs = os.Getenv("DRONE_PULL_REQUEST")
	if prs == "" {
		// not a pull-request build
		return nil, false, nil
	}
	pr, err := strconv.Atoi(prs)
	if err != nil {
		return nil, true, fmt.Errorf("unexpected env variable (DRONE_PULL_REQUEST): %v", prs)
	}
	reposlug, err := nonEmptyEnv("DRONE_REPO") // e.g. haya14busa/reviewdog
	if err != nil {
		return nil, true, err
	}
	rss := strings.SplitN(reposlug, "/", 2)
	if len(rss) < 2 {
		return nil, true, fmt.Errorf("unexpected env variable. DRONE_REPO=%v", reposlug)
	}
	owner, repo := rss[0], rss[1]
	sha, err := nonEmptyEnv("DRONE_COMMIT")
	if err != nil {
		return nil, true, err
	}
	g = &GitHubPR{
		owner: owner,
		repo:  repo,
		pr:    pr,
		sha:   sha,
	}
	return g, true, nil
}

func commonci() (g *GitHubPR, isPR bool, err error) {
	var prs string // pull request number in string
	prs = os.Getenv("CI_PULL_REQUEST")
	if prs == "" {
		// not a pull-request build
		return nil, false, nil
	}
	pr, err := strconv.Atoi(prs)
	if err != nil {
		return nil, true, fmt.Errorf("unexpected env variable (CI_PULL_REQUEST): %v", prs)
	}
	owner, err := nonEmptyEnv("CI_REPO_OWNER")
	if err != nil {
		return nil, true, err
	}
	repo, err := nonEmptyEnv("CI_REPO_NAME")
	if err != nil {
		return nil, true, err
	}
	sha, err := nonEmptyEnv("CI_COMMIT")
	if err != nil {
		return nil, true, err
	}
	g = &GitHubPR{
		owner: owner,
		repo:  repo,
		pr:    pr,
		sha:   sha,
	}
	return g, true, nil
}

// GitHubPR represents required information about GitHub PullRequest.
type GitHubPR struct {
	owner string
	repo  string
	pr    int
	sha   string
}

func nonEmptyEnv(env string) (string, error) {
	v := os.Getenv(env)
	if v == "" {
		return "", fmt.Errorf("environment variable $%v is not set", env)
	}
	return v, nil
}

type strslice []string

func (ss *strslice) String() string {
	return fmt.Sprintf("%v", *ss)
}

func (ss *strslice) Set(value string) error {
	*ss = append(*ss, value)
	return nil
}