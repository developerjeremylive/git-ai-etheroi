package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/policyeval"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/sdk"
)

// policySynopsis is the command line of graft policy.
const policySynopsis = "usage: graft policy check FILE [-repo REPO]... [--limit N] [--json]"

// policyUsage lists the subcommands of graft policy.
const policyUsage = "policy requires a subcommand: check (" + policySynopsis + ")"

// policyHelp explains graft policy check below its synopsis.
const policyHelp = `Checks the standalone access policy ([[graft.policy]]) of the configuration file
FILE before a server reloads it (SIGUSR1). It changes nothing.

- Always: FILE is read as a server reads it, its policy is validated as a
  reload validates it, and its rules are printed with their numbers and the
  policy's SHA-256 (the server exports it as graft_policy_info). An invalid
  policy exits 2: a reload would reject it and keep the policy in force. The
  rest of the file is not validated: a reload also checks it against the
  server's paths and settings, and rejects a file that fails there.
- With -repo REPO (repeatable) and the server's address: the last N
  changesets of each repository (--limit, default 1000) are read with the
  token, and each reference update is evaluated with the candidate policy as
  the actor that made it; for a batched land, each member's actor. The updates
  the candidate would deny are printed with the reason, by actor and reference.

What it cannot show: an update that the policy in force refused is not in the
changeset log. The check lists what the candidate would newly deny among what
happened, not what it would newly allow. A batched land whose list of members
the log cut short is checked for the members it lists.`

// defaultPolicyCheckLimit is the number of changesets of each repository that
// graft policy check reads by default.
const defaultPolicyCheckLimit = 1000

// policyFlags holds the flags of graft policy check.
type policyFlags struct {
	repos stringList
	limit int
}

// policyRule is a rule of the checked policy, numbered from 1 as the server's
// messages number them.
type policyRule struct {
	Number int      `json:"number"`
	Actor  string   `json:"actor"`
	Refs   []string `json:"refs"`
	Allow  bool     `json:"allow"`
}

// policyDenial is a reference update of the changeset log that the checked
// policy would deny, with the changesets that made it.
type policyDenial struct {
	Actor     string   `json:"actor"`
	Reference string   `json:"reference"`
	Reason    string   `json:"reason"`
	Sequences []uint64 `json:"sequences"`
}

// policyRepository is the check of one repository's changesets.
type policyRepository struct {
	Repository string `json:"repository"`
	// Checked is the number of changesets read, FirstSequence and LastSequence
	// their range (0 without changesets).
	Checked       int            `json:"checked"`
	FirstSequence uint64         `json:"first_sequence"`
	LastSequence  uint64         `json:"last_sequence"`
	Denied        []policyDenial `json:"denied"`
}

// policyCheckResult is the JSON output of graft policy check.
type policyCheckResult struct {
	File         string             `json:"file"`
	SHA256       string             `json:"sha256"`
	Rules        []policyRule       `json:"rules"`
	Repositories []policyRepository `json:"repositories,omitempty"`
}

// policyCommand returns the policy command, the operator's check of a policy
// before a reload.
func policyCommand() command {
	f := &policyFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\n%s\n\nflags:\n", policySynopsis, policyHelp)
				fs.PrintDefaults()
			}
			fs.Var(&f.repos, "repo", "check the candidate against the recent changesets of this repository (may be repeated)")
			fs.IntVar(&f.limit, "limit", defaultPolicyCheckLimit, "the number of most recent changesets of each repository to check")
			fs.BoolVar(&o.jsonOut, "json", false, "print the result as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdPolicy(ctx, out, o, f, args)
		},
	}
}

// cmdPolicy dispatches a policy subcommand. Arguments are checked before the
// server is contacted.
func cmdPolicy(ctx context.Context, out io.Writer, o *options, f *policyFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", policyUsage)
	}
	if args[0] != "check" {
		return usagef("unknown policy subcommand %q: %s", args[0], policyUsage)
	}
	if len(args) != 2 {
		return usagef("%s", policySynopsis)
	}
	if f.limit <= 0 {
		return usagef("--limit must be positive, got %d", f.limit)
	}
	if len(f.repos) > 0 && o.address == "" {
		return usagef("policy check -repo needs the server address: -address or GRAFT_ADDRESS")
	}
	return policyCheck(ctx, out, o, f, args[1])
}

// policyCheck checks the policy of the configuration file at path.
func policyCheck(ctx context.Context, out io.Writer, o *options, f *policyFlags, path string) error {
	graft, err := loadPolicy(path)
	if err != nil {
		// A reload would reject the file: the exit status of a usage error.
		return usagef("%s: %v", path, err)
	}

	res := policyCheckResult{File: path, SHA256: policystore.Hash(graft.Policy), Rules: []policyRule{}}
	for i, rule := range graft.Policy {
		refs := rule.Refs
		if refs == nil {
			refs = []string{}
		}
		res.Rules = append(res.Rules, policyRule{Number: i + 1, Actor: rule.Actor, Refs: refs, Allow: rule.Allow})
	}

	if len(f.repos) > 0 {
		dialOpts := []sdk.DialOption{sdk.WithStorage(pick(o.storage, defaultStorage))}
		if o.token != "" {
			dialOpts = append(dialOpts, sdk.WithToken(o.token))
		}
		client, err := sdk.Dial(ctx, o.address, dialOpts...)
		if err != nil {
			return err
		}
		registerCloser(ctx, client)

		for _, relativePath := range f.repos {
			checked, err := policyCheckRepository(ctx, client.Repo(relativePath), graft, f.limit)
			if err != nil {
				return fmt.Errorf("repository %s: %w", relativePath, err)
			}
			checked.Repository = relativePath
			res.Repositories = append(res.Repositories, checked)
		}
	}

	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printPolicyCheck(out, res)
	return nil
}

// loadPolicy reads the configuration file at path as a server reads it and
// validates its [graft] section as a reload does.
func loadPolicy(path string) (config.Graft, error) {
	file, err := os.Open(path)
	if err != nil {
		return config.Graft{}, err
	}
	defer file.Close()

	cfg, err := config.Load(file)
	if err != nil {
		return config.Graft{}, err
	}
	if err := cfg.Graft.Validate(); err != nil {
		return config.Graft{}, err
	}
	return cfg.Graft, nil
}

// policyCheckRepository evaluates graft with the reference updates of the last
// limit changesets of repo.
func policyCheckRepository(ctx context.Context, repo *sdk.Repo, graft config.Graft, limit int) (policyRepository, error) {
	changesets, err := lastChangesets(ctx, repo, limit)
	if err != nil {
		return policyRepository{}, err
	}

	res := policyRepository{Checked: len(changesets), Denied: []policyDenial{}}
	if len(changesets) > 0 {
		res.FirstSequence = changesets[0].GetSequence()
		res.LastSequence = changesets[len(changesets)-1].GetSequence()
	}

	type key struct{ actor, reference, reason string }
	denied := map[key][]uint64{}
	for _, cs := range changesets {
		actors := []string{cs.GetActor()}
		if members := cs.GetMembers(); len(members) > 0 {
			actors = actors[:0]
			for _, member := range members {
				actors = append(actors, member.GetActor())
			}
		}
		reference := string(cs.GetReference())
		for _, actor := range actors {
			allowed, reason, err := policyeval.EvaluateUpdate(graft.Policy, actor, reference, cs.GetOldOid(), cs.GetNewOid())
			if err != nil {
				return policyRepository{}, fmt.Errorf("changeset %d: %w", cs.GetSequence(), err)
			}
			if !allowed {
				k := key{actor: actor, reference: reference, reason: reason}
				denied[k] = append(denied[k], cs.GetSequence())
			}
		}
	}

	for k, sequences := range denied {
		res.Denied = append(res.Denied, policyDenial{Actor: k.actor, Reference: k.reference, Reason: k.reason, Sequences: sequences})
	}
	sort.Slice(res.Denied, func(i, j int) bool {
		a, b := res.Denied[i], res.Denied[j]
		if a.Actor != b.Actor {
			return a.Actor < b.Actor
		}
		if a.Reference != b.Reference {
			return a.Reference < b.Reference
		}
		return a.Reason < b.Reason
	})
	return res, nil
}

// lastChangesets returns the last limit changesets of repo, in ascending
// order. The last sequence is found by a search of the cursors that still
// return a changeset (a cursor does exactly when a later sequence exists), one
// call of a single changeset per step, before the changesets are read. The log
// is dense (sequences start at 1 and have no gaps), so the changesets after
// last-limit are the last limit ones.
func lastChangesets(ctx context.Context, repo *sdk.Repo, limit int) ([]*sdk.Changeset, error) {
	exists := func(after uint64) (bool, error) {
		changesets, err := repo.Changesets(ctx, after, 1)
		return len(changesets) > 0, err
	}

	// last is a sequence that exists (0: none checked yet), beyond one that does not.
	var last, beyond uint64 = 0, 1
	for {
		ok, err := exists(beyond - 1)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		last = beyond
		beyond *= 2
	}
	for last+1 < beyond {
		middle := last + (beyond-last)/2
		ok, err := exists(middle - 1)
		if err != nil {
			return nil, err
		}
		if ok {
			last = middle
		} else {
			beyond = middle
		}
	}
	if last == 0 {
		return nil, nil
	}

	var after uint64
	if last > uint64(limit) {
		after = last - uint64(limit)
	}
	var changesets []*sdk.Changeset
	for after < last {
		page, err := repo.Changesets(ctx, after, uint32(min(last-after, defaultPolicyCheckLimit)))
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		changesets = append(changesets, page...)
		after = page[len(page)-1].GetSequence()
	}
	return changesets, nil
}

// printPolicyCheck prints the result of graft policy check for a reader.
func printPolicyCheck(out io.Writer, res policyCheckResult) {
	fmt.Fprintf(out, "policy of %s is valid: %d rules, sha256 %s\n", res.File, len(res.Rules), res.SHA256)
	for _, rule := range res.Rules {
		verdict := "deny"
		if rule.Allow {
			verdict = "allow"
		}
		refs := "every reference"
		if len(rule.Refs) > 0 {
			refs = strings.Join(rule.Refs, ", ")
		}
		actor := rule.Actor
		if actor == "" {
			actor = "*"
		}
		fmt.Fprintf(out, "  %d. %s %s: %s\n", rule.Number, verdict, actor, refs)
	}

	for _, repo := range res.Repositories {
		if repo.Checked == 0 {
			fmt.Fprintf(out, "%s: no changesets\n", repo.Repository)
			continue
		}
		fmt.Fprintf(out, "%s: %d changesets (%d..%d), %d updates the candidate would deny\n",
			repo.Repository, repo.Checked, repo.FirstSequence, repo.LastSequence, len(repo.Denied))
		for _, denial := range repo.Denied {
			fmt.Fprintf(out, "  %s %s (%d changesets, last %d): %s\n",
				denial.Actor, denial.Reference, len(denial.Sequences), denial.Sequences[len(denial.Sequences)-1], denial.Reason)
		}
	}
}
