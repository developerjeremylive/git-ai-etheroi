package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/sdk"
)

// repoSynopsis is the command line of graft repo.
const repoSynopsis = "usage: graft repo create REPO [--default-branch NAME] [--object-format sha1|sha256] [--initial-commit [--branch NAME]...] [--json]"

// repoUsage lists the subcommands of graft repo.
const repoUsage = "repo requires a subcommand: create (" + repoSynopsis + ")"

// repoHelp explains graft repo create below its synopsis.
const repoHelp = `Creates REPO, or completes it: the command converges on the requested state, so
running it again after a partial failure finishes the work and changes nothing
that is done already.

- A missing repository is created. An existing one is AlreadyExists, unless
  --initial-commit is given.
- --initial-commit makes the first commit (README.md) on the default branch,
  so that the repository can be cloned. On an existing repository, HEAD must
  point to the default branch; the commit is made only when the repository
  has no reference at all, and is not needed when the default branch exists.
  A repository with other references but without the default branch is an
  error.
- --branch NAME creates NAME at the tip of the default branch, unless it
  exists.

Creating a repository needs the shared (operator) token. The initial commit
and the branches pass the server's policy as the user of GRAFT_USER (-user).
With the storage core in shadow mode the new repository is not tracked until
graft storage import -repo REPO.`

// initialCommitMessage is the message of the commit of --initial-commit.
const initialCommitMessage = "Initial commit"

// repoFlags holds the flags of graft repo create.
type repoFlags struct {
	defaultBranch string
	objectFormat  string
	initialCommit bool
	branches      stringList
}

// stringList is a flag that may be given several times.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// repoCreateResult is the JSON output of graft repo create.
type repoCreateResult struct {
	Repository string `json:"repository"`
	Storage    string `json:"storage"`
	// Created is false when the repository existed already.
	Created       bool   `json:"created"`
	DefaultBranch string `json:"default_branch"`
	// InitialCommit is the tip of the default branch with --initial-commit,
	// and InitialCommitCreated whether this run made it.
	InitialCommit        string   `json:"initial_commit,omitempty"`
	InitialCommitCreated bool     `json:"initial_commit_created,omitempty"`
	BranchesCreated      []string `json:"branches_created,omitempty"`
	BranchesExisting     []string `json:"branches_existing,omitempty"`
	// Untracked is true when the storage core runs in shadow mode and does not
	// track the repository yet (graft storage import).
	Untracked bool `json:"untracked,omitempty"`
}

// repoCommand returns the repo command, the operator's creation of
// repositories.
func repoCommand() command {
	f := &repoFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\n%s\n\nflags:\n", repoSynopsis, repoHelp)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.defaultBranch, "default-branch", "main", "the branch HEAD points to, and the branch of the initial commit")
			fs.StringVar(&f.objectFormat, "object-format", "", "the object hash of a new repository, sha1 or sha256 (default: the server's)")
			fs.BoolVar(&f.initialCommit, "initial-commit", false, "make sure the default branch has a commit: make the first commit (README.md) of a repository without references")
			fs.Var(&f.branches, "branch", "with --initial-commit, make sure this branch exists: create it at the tip of the default branch (may be repeated)")
			fs.BoolVar(&o.jsonOut, "json", false, "print the result as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdRepo(ctx, out, o, f, args)
		},
	}
}

// cmdRepo dispatches a repo subcommand. Arguments are checked before the
// server is contacted.
func cmdRepo(ctx context.Context, out io.Writer, o *options, f *repoFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", repoUsage)
	}
	if args[0] != "create" {
		return usagef("unknown repo subcommand %q: %s", args[0], repoUsage)
	}
	return repoCreate(ctx, out, o, f, args[1:])
}

// repoProgress records what a run of graft repo create did and what it has
// left, for the error of a partial failure.
type repoProgress struct {
	done, left []string
}

// fail reports err with what was done and what is left. A denial of the
// server's policy says so: running the command again is denied the same way.
func (p *repoProgress) fail(relativePath string, err error) error {
	report := ""
	if len(p.done) > 0 {
		report += "\ndone: " + strings.Join(p.done, "; ")
	}
	if len(p.left) > 0 {
		report += "\nleft: " + strings.Join(p.left, "; ")
	}
	if sdk.Code(err) == codes.PermissionDenied {
		return fmt.Errorf("repository %s: denied by the server: %w%s\nthe same command is denied again until the policy allows the user of GRAFT_USER (-user) to write these branches", displayText(relativePath), err, report)
	}
	return fmt.Errorf("repository %s: %w%s\nrun the same command again to finish", displayText(relativePath), err, report)
}

func repoCreate(ctx context.Context, out io.Writer, o *options, f *repoFlags, args []string) error {
	if len(args) != 1 || args[0] == "" {
		return usagef("repo create takes one repository path: %s", repoSynopsis)
	}
	relativePath := args[0]
	defaultBranch, branches, err := repoBranches(f)
	if err != nil {
		return err
	}
	switch {
	case f.objectFormat != "" && f.objectFormat != "sha1" && f.objectFormat != "sha256":
		return usagef("repo create: --object-format must be sha1 or sha256, not %q", f.objectFormat)
	case len(f.branches) > 0 && !f.initialCommit:
		return usagef("repo create: --branch creates a branch at the initial commit and requires --initial-commit")
	case o.address == "":
		return usagef("repo create needs the server address: -address or GRAFT_ADDRESS")
	}
	author, err := parseAuthor(o.author)
	if err != nil {
		return err
	}

	dialOpts := []sdk.DialOption{sdk.WithStorage(pick(o.storage, defaultStorage))}
	if o.token != "" {
		dialOpts = append(dialOpts, sdk.WithToken(o.token))
	}
	client, err := sdk.Dial(ctx, o.address, dialOpts...)
	if err != nil {
		return err
	}
	registerCloser(ctx, client)
	repo := client.Repo(relativePath)

	res := repoCreateResult{Repository: relativePath, Storage: client.Storage(), DefaultBranch: defaultBranch}
	progress := &repoProgress{}
	if f.initialCommit {
		progress.left = append(progress.left, "initial commit on "+defaultBranch)
	}
	for _, branch := range branches {
		progress.left = append(progress.left, "branch "+branch)
	}

	err = repo.Create(ctx, sdk.CreateOptions{DefaultBranch: defaultBranch, ObjectFormat: f.objectFormat})
	switch {
	case err == nil:
		res.Created = true
		progress.done = append(progress.done, "created the repository")
		// The storage core's mode is best effort: a server that cannot report
		// it (an older one) gets the hint of the help text only.
		if state, stateErr := repo.StorageState(ctx); stateErr == nil && state.Mode == "shadow" && !state.Tracked {
			res.Untracked = true
		}
	case sdk.IsAlreadyExists(err) && f.initialCommit:
		// Complete the existing repository below.
	case sdk.IsAlreadyExists(err):
		return fmt.Errorf("repository %s exists already", displayText(relativePath))
	default:
		return withOperatorHint(err)
	}

	if f.initialCommit {
		user := sdk.User{ID: pick(o.user, anonymousUser), Username: pick(o.user, anonymousUser), Name: author.Name, Email: author.Email}
		tip, created, err := ensureInitialCommit(ctx, repo, defaultBranch, user, author)
		if err != nil {
			return progress.fail(relativePath, err)
		}
		res.InitialCommit, res.InitialCommitCreated = tip, created
		progress.left = progress.left[1:]
		progress.done = append(progress.done, "initial commit "+tip+" on "+defaultBranch)

		for _, branch := range branches {
			created, err := ensureBranch(ctx, repo, branch, tip, user)
			if err != nil {
				return progress.fail(relativePath, err)
			}
			if created {
				res.BranchesCreated = append(res.BranchesCreated, branch)
			} else {
				res.BranchesExisting = append(res.BranchesExisting, branch)
			}
			progress.left = progress.left[1:]
			progress.done = append(progress.done, "branch "+branch)
		}
	}

	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	if res.Created {
		fmt.Fprintf(out, "created repository %s on storage %s\n", displayText(relativePath), displayText(res.Storage))
	} else {
		fmt.Fprintf(out, "repository %s exists\n", displayText(relativePath))
	}
	if res.InitialCommit != "" {
		verb := "is"
		if res.InitialCommitCreated {
			verb = "created as"
		}
		fmt.Fprintf(out, "%s %s %s\n", displayText(defaultBranch), verb, res.InitialCommit)
	}
	for _, branch := range res.BranchesCreated {
		fmt.Fprintf(out, "created branch %s at %s\n", displayText(branch), res.InitialCommit)
	}
	for _, branch := range res.BranchesExisting {
		fmt.Fprintf(out, "branch %s exists\n", displayText(branch))
	}
	if res.Untracked {
		fmt.Fprintf(out, "the storage core runs in shadow mode: run graft storage import -repo %s to track the repository\n", displayText(relativePath))
	}
	return nil
}

// repoBranches validates the branch names of graft repo create with git's
// reference name rules, before any server call, and returns the default branch
// and the extra branches without duplicates and without the default branch.
func repoBranches(f *repoFlags) (string, []string, error) {
	defaultBranch := strings.TrimPrefix(f.defaultBranch, "refs/heads/")
	if err := git.ValidateReference("refs/heads/" + defaultBranch); err != nil || defaultBranch == "" {
		return "", nil, usagef("repo create: invalid --default-branch %q", f.defaultBranch)
	}
	var branches []string
	for _, branch := range f.branches {
		name := strings.TrimPrefix(branch, "refs/heads/")
		if err := git.ValidateReference("refs/heads/" + name); err != nil || name == "" {
			return "", nil, usagef("repo create: invalid --branch %q", branch)
		}
		if name != defaultBranch && !slices.Contains(branches, name) {
			branches = append(branches, name)
		}
	}
	return defaultBranch, branches, nil
}

// ensureInitialCommit makes sure the default branch of repo has a commit and
// returns its tip, and whether this call made the commit. HEAD must point to
// the default branch. The commit is made only in a repository without any
// reference, and only if the default branch still does not exist when it is
// written (ExpectedOldOID is the zero object ID): a concurrent run that made it
// first is found by reading the branch again.
func ensureInitialCommit(ctx context.Context, repo *sdk.Repo, defaultBranch string, user sdk.User, author workspace.Signature) (string, bool, error) {
	ref := "refs/heads/" + defaultBranch
	head, err := repo.Head(ctx)
	if err != nil {
		return "", false, fmt.Errorf("reading HEAD: %w", err)
	}
	if head != ref {
		return "", false, fmt.Errorf("HEAD points to %s, not %s: pass --default-branch %s, or no --initial-commit", displayText(head), displayText(ref), displayText(strings.TrimPrefix(head, "refs/heads/")))
	}

	if tip, ok, err := branchTip(ctx, repo, ref); err != nil || ok {
		return tip, false, err
	}
	hasReferences, err := repo.HasReferences(ctx)
	if err != nil {
		return "", false, fmt.Errorf("listing references: %w", err)
	}
	if hasReferences {
		// A concurrent run may have made the initial commit since the branch was
		// read: its references are the default branch's.
		if tip, ok, err := branchTip(ctx, repo, ref); err != nil || ok {
			return tip, false, err
		}
		return "", false, fmt.Errorf("the repository has references but no %s: it is not empty, so no initial commit is made", displayText(ref))
	}

	zero, err := repo.ZeroOID(ctx)
	if err != nil {
		return "", false, fmt.Errorf("reading the object format: %w", err)
	}
	commit, commitErr := repo.CommitFiles(ctx, sdk.CommitOptions{
		Branch:         defaultBranch,
		ExpectedOldOID: zero,
		Message:        initialCommitMessage,
		Author:         sdk.Signature{Name: author.Name, Email: author.Email},
		User:           user,
		Actions:        []sdk.Action{sdk.Put{Path: "README.md", Content: []byte("# " + repositoryName(repo.RelativePath()) + "\n")}},
	})
	if commitErr == nil {
		return commit.CommitID, true, nil
	}
	if tip, ok, err := branchTip(ctx, repo, ref); err == nil && ok {
		return tip, false, nil
	}
	return "", false, fmt.Errorf("initial commit on %s: %w", displayText(defaultBranch), commitErr)
}

// ensureBranch makes sure branch exists, creating it at tip, and reports
// whether this call created it. A branch created concurrently is found by
// checking again after a failed creation.
func ensureBranch(ctx context.Context, repo *sdk.Repo, branch, tip string, user sdk.User) (bool, error) {
	ref := "refs/heads/" + branch
	exists, err := repo.ReferenceExists(ctx, ref)
	if err != nil {
		return false, fmt.Errorf("checking branch %s: %w", displayText(branch), err)
	}
	if exists {
		return false, nil
	}
	createErr := repo.CreateBranch(ctx, sdk.CreateBranchOptions{Branch: branch, StartPoint: tip, User: user})
	if createErr == nil {
		return true, nil
	}
	if exists, err := repo.ReferenceExists(ctx, ref); err == nil && exists {
		return false, nil
	}
	return false, fmt.Errorf("creating branch %s: %w", displayText(branch), createErr)
}

// branchTip returns the commit of the branch ref (a full name), read by its
// exact name, and whether it exists.
func branchTip(ctx context.Context, repo *sdk.Repo, ref string) (string, bool, error) {
	exists, err := repo.ReferenceExists(ctx, ref)
	if err != nil || !exists {
		return "", false, err
	}
	tip, err := repo.ResolveRevision(ctx, ref)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", displayText(ref), err)
	}
	return tip, true, nil
}

// repositoryName is the name of the repository at relativePath, for its
// README: the last path element without .git.
func repositoryName(relativePath string) string {
	return strings.TrimSuffix(path.Base(relativePath), ".git")
}

// withOperatorHint explains a refused creation: agents may never create
// repositories.
func withOperatorHint(err error) error {
	if sdk.Code(err) != codes.PermissionDenied {
		return err
	}
	return fmt.Errorf("%w\nhint: creating a repository needs the shared (operator) token in GRAFT_TOKEN (or -token), not an agent token", err)
}
