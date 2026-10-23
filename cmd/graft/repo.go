package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/sdk"
)

// repoSynopsis is the command line of graft repo.
const repoSynopsis = "usage: graft repo create REPO [--default-branch NAME] [--object-format sha1|sha256] [--initial-commit [--branch NAME]...] [--json]"

// repoUsage lists the subcommands of graft repo.
const repoUsage = "repo requires a subcommand: create (" + repoSynopsis + ")"

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
	// Created is false when the repository existed already, empty, and the
	// command only added the initial commit.
	Created       bool     `json:"created"`
	DefaultBranch string   `json:"default_branch"`
	InitialCommit string   `json:"initial_commit,omitempty"`
	Branches      []string `json:"branches,omitempty"`
}

// repoCommand returns the repo command, the operator's creation of
// repositories.
func repoCommand() command {
	f := &repoFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\nCreating a repository needs the shared (operator) token.\n\nflags:\n", repoSynopsis)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.defaultBranch, "default-branch", "main", "the branch HEAD points to, and the branch of the initial commit")
			fs.StringVar(&f.objectFormat, "object-format", "", "the object hash, sha1 or sha256 (default: the server's)")
			fs.BoolVar(&f.initialCommit, "initial-commit", false, "also make a first commit (README.md) on the default branch, so that the repository can be cloned; on an existing repository without branches, only make that commit")
			fs.Var(&f.branches, "branch", "with --initial-commit, also create this branch at the initial commit (may be repeated)")
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

func repoCreate(ctx context.Context, out io.Writer, o *options, f *repoFlags, args []string) error {
	if len(args) != 1 || args[0] == "" {
		return usagef("repo create takes one repository path: %s", repoSynopsis)
	}
	relativePath := args[0]
	defaultBranch := strings.TrimPrefix(f.defaultBranch, "refs/heads/")
	switch {
	case defaultBranch == "":
		return usagef("repo create: --default-branch must not be empty")
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

	res := repoCreateResult{Repository: relativePath, Storage: client.Storage(), Created: true, DefaultBranch: defaultBranch}
	err = repo.Create(ctx, sdk.CreateOptions{DefaultBranch: defaultBranch, ObjectFormat: f.objectFormat})
	switch {
	case sdk.IsAlreadyExists(err) && f.initialCommit:
		// A repository without branches is the one an earlier run created before
		// its initial commit failed: make that commit now.
		hasBranches, checkErr := repo.HasBranches(ctx)
		if checkErr != nil {
			return withOperatorHint(checkErr)
		}
		if hasBranches {
			return fmt.Errorf("repository %s exists already and has branches", displayText(relativePath))
		}
		res.Created = false
	case sdk.IsAlreadyExists(err):
		return fmt.Errorf("repository %s exists already", displayText(relativePath))
	case err != nil:
		return withOperatorHint(err)
	}

	if f.initialCommit {
		user := sdk.User{ID: pick(o.user, anonymousUser), Username: pick(o.user, anonymousUser), Name: author.Name, Email: author.Email}
		commit, err := repo.CommitFiles(ctx, sdk.CommitOptions{
			Branch:  defaultBranch,
			Message: initialCommitMessage,
			Author:  sdk.Signature{Name: author.Name, Email: author.Email},
			User:    user,
			Actions: []sdk.Action{sdk.Put{Path: "README.md", Content: []byte("# " + repositoryName(relativePath) + "\n")}},
		})
		if err != nil {
			return fmt.Errorf("repository %s has no initial commit: %w (run the same command again to add it)", displayText(relativePath), withUserHint(err, o.user))
		}
		res.InitialCommit = commit.CommitID

		for _, branch := range f.branches {
			if _, err := repo.Land(ctx, sdk.LandOptions{Branch: branch, Commit: commit.CommitID, CreateBranch: true, User: user}); err != nil {
				return fmt.Errorf("creating branch %s at the initial commit %s: %w", displayText(branch), commit.CommitID, withUserHint(err, o.user))
			}
			res.Branches = append(res.Branches, branch)
		}
	}

	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	if res.Created {
		fmt.Fprintf(out, "created repository %s on storage %s\n", displayText(relativePath), displayText(res.Storage))
	} else {
		fmt.Fprintf(out, "repository %s exists without branches: adding the initial commit\n", displayText(relativePath))
	}
	if res.InitialCommit != "" {
		fmt.Fprintf(out, "initial commit %s on %s\n", res.InitialCommit, displayText(defaultBranch))
	}
	for _, branch := range res.Branches {
		fmt.Fprintf(out, "created branch %s at %s\n", displayText(branch), res.InitialCommit)
	}
	return nil
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
