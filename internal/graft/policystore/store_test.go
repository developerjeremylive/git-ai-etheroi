package policystore

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

var (
	allowMain = []config.GraftPolicyRule{{Actor: "planner", Refs: []string{"refs/heads/main"}, Allow: true}}
	denyAll   = []config.GraftPolicyRule{{Actor: "*", Allow: false}}
)

func TestStore_startupPolicy(t *testing.T) {
	t.Parallel()

	s := New(allowMain)
	current := s.Current()
	require.Equal(t, allowMain, current.Rules)
	require.EqualValues(t, 1, current.Generation)
	require.Equal(t, Hash(allowMain), current.SHA256)
	require.Equal(t, 1.0, testutil.ToFloat64(s.generation))
	require.Equal(t, 1.0, testutil.ToFloat64(s.info.WithLabelValues(current.SHA256)))

	// The snapshot does not share the caller's slices.
	rules := []config.GraftPolicyRule{{Actor: "a", Refs: []string{"refs/heads/x"}, Allow: true}}
	s = New(rules)
	rules[0].Refs[0] = "refs/heads/y"
	require.Equal(t, "refs/heads/x", s.Current().Rules[0].Refs[0])
}

func TestStore_reload(t *testing.T) {
	t.Parallel()

	running := config.Graft{Standalone: true, Agents: []config.GraftAgent{{Name: "planner", Token: "t"}}, Policy: allowMain}
	s := New(running.Policy)
	first := s.Current()

	t.Run("same rules, another file layout", func(t *testing.T) {
		next := running
		next.Policy = []config.GraftPolicyRule{{Actor: "planner", Refs: []string{"refs/heads/main"}, Allow: true}}
		outcome, err := s.Reload(next, running)
		require.NoError(t, err)
		require.Equal(t, Unchanged, outcome.Result)
		require.Same(t, first, s.Current())
	})

	t.Run("an invalid policy keeps the one in force", func(t *testing.T) {
		for name, rules := range map[string][]config.GraftPolicyRule{
			"bad actor glob": {{Actor: "[", Allow: true}},
			"bad refs glob":  {{Refs: []string{"refs/heads/["}, Allow: true}},
			"empty refs":     {{Refs: []string{""}, Allow: false}},
		} {
			next := running
			next.Policy = rules
			outcome, err := s.Reload(next, running)
			require.Error(t, err, name)
			require.Equal(t, Rejected, outcome.Result, name)
			require.Same(t, first, s.Current(), name)
		}
		require.Equal(t, 3.0, testutil.ToFloat64(s.reloads.WithLabelValues(string(Rejected))))
	})

	t.Run("a new policy is applied", func(t *testing.T) {
		next := running
		next.Policy = denyAll
		outcome, err := s.Reload(next, running)
		require.NoError(t, err)
		require.Equal(t, Applied, outcome.Result)
		require.Same(t, first, outcome.Previous)
		require.Equal(t, denyAll, s.Current().Rules)
		require.EqualValues(t, 2, s.Current().Generation)
		require.Empty(t, outcome.NeedsRestart)
		require.Equal(t, 2.0, testutil.ToFloat64(s.generation))
		require.Equal(t, 1.0, testutil.ToFloat64(s.info.WithLabelValues(Hash(denyAll))))
		require.Equal(t, 1, testutil.CollectAndCount(s.info), "only the policy in force is reported")
	})

	t.Run("other changes need a restart and are not applied", func(t *testing.T) {
		next := running
		next.Policy = allowMain
		next.Agents = []config.GraftAgent{{Name: "planner", Token: "t"}, {Name: "builder", Token: "u"}}
		next.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
		outcome, err := s.Reload(next, running)
		require.NoError(t, err)
		require.Equal(t, Applied, outcome.Result)
		require.Equal(t, []string{"graft.agent", "graft.agent_rpcs"}, outcome.NeedsRestart)
		require.EqualValues(t, 3, s.Current().Generation)
	})

	t.Run("actors that name no agent are reported", func(t *testing.T) {
		next := running
		next.Policy = []config.GraftPolicyRule{
			{Actor: "planner", Allow: true},
			{Actor: "builder", Allow: true},
			{Actor: "bot-*", Allow: true},
			{Actor: "builder", Allow: false},
			{Allow: false},
		}
		outcome, err := s.Reload(next, running)
		require.NoError(t, err)
		require.Equal(t, []string{"builder"}, outcome.UnknownActors)

		// Without agents every actor is a user ID.
		require.Empty(t, unknownActors(next.Policy, nil))
	})
}

func TestStore_reject(t *testing.T) {
	t.Parallel()

	s := New(allowMain)
	first := s.Current()
	outcome := s.Reject()
	require.Equal(t, Rejected, outcome.Result)
	require.Same(t, first, outcome.Current)
	require.Same(t, first, s.Current())
	require.Equal(t, 1.0, testutil.ToFloat64(s.reloads.WithLabelValues(string(Rejected))))
}

// TestStore_concurrentReadersAndReloads reads the policy while it is reloaded, valid and invalid
// policies alternating: a reader never sees nil, an empty policy or a generation that goes back,
// and every snapshot it sees is one of the published policies, whole.
func TestStore_concurrentReadersAndReloads(t *testing.T) {
	t.Parallel()

	running := config.Graft{Standalone: true, Policy: allowMain}
	s := New(allowMain)
	valid := map[string][]config.GraftPolicyRule{Hash(allowMain): allowMain, Hash(denyAll): denyAll}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan string, 8)
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				current := s.Current()
				switch {
				case current == nil:
					errs <- "nil snapshot"
					return
				case len(current.Rules) == 0:
					errs <- "empty policy"
					return
				case current.Generation < last:
					errs <- "generation went back"
					return
				}
				rules, ok := valid[current.SHA256]
				if !ok || Hash(current.Rules) != current.SHA256 || len(rules) != len(current.Rules) {
					errs <- "a snapshot that was never published whole"
					return
				}
				last = current.Generation
			}
		}()
	}

	invalid := running
	invalid.Policy = []config.GraftPolicyRule{{Refs: []string{""}}}
	badGlob := running
	badGlob.Policy = []config.GraftPolicyRule{{Actor: "[", Allow: true}}
	for i := 0; i < 500; i++ {
		next := running
		next.Policy = allowMain
		if i%2 == 1 {
			next.Policy = denyAll
		}
		_, err := s.Reload(next, running)
		require.NoError(t, err)
		_, err = s.Reload(invalid, running)
		require.Error(t, err)
		_, err = s.Reload(badGlob, running)
		require.Error(t, err)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for e := range errs {
		require.Fail(t, e)
	}
	// The first reload has the startup policy (unchanged); the 499 after it alternate.
	require.EqualValues(t, 500, s.Current().Generation)
}

func TestHash(t *testing.T) {
	t.Parallel()

	require.Equal(t, Hash([]config.GraftPolicyRule{{Actor: "a"}}), Hash([]config.GraftPolicyRule{{Actor: "a", Refs: []string{}}}))
	require.NotEqual(t, Hash(allowMain), Hash(denyAll))
	require.NotEqual(t, Hash([]config.GraftPolicyRule{{Actor: "a"}, {Actor: "b"}}), Hash([]config.GraftPolicyRule{{Actor: "b"}, {Actor: "a"}}), "order matters")
	require.Len(t, Hash(nil), 64)
	require.False(t, strings.ContainsAny(Hash(allowMain), "ABCDEF"))
}

func TestAdmissionGeneration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.Zero(t, AdmissionGeneration(ctx))
	require.EqualValues(t, 7, AdmissionGeneration(WithAdmissionGeneration(ctx, 7)))
}
