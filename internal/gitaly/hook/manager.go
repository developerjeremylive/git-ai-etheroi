package hook

import (
	"context"
	"io"
	"time"

	"github.com/feder-cr/git-ai/internal/cgroups"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/eager"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
)

// ReferenceTransactionState is the state of the Git reference transaction. It reflects the first
// parameter of the reference-transaction hook. See githooks(1) for more information.
type ReferenceTransactionState int

const (
	// ReferenceTransactionPrepared indicates all reference updates have been queued to the
	// transaction and were locked on disk.
	ReferenceTransactionPrepared = ReferenceTransactionState(iota)
	// ReferenceTransactionCommitted indicates the reference transaction was committed and all
	// references now have their respective new value.
	ReferenceTransactionCommitted
	// ReferenceTransactionAborted indicates the transaction was aborted, no changes were
	// performed and the reference locks have been released.
	ReferenceTransactionAborted
	// ReferenceTransactionPreparing indicates all reference updates are ready to be locked
	ReferenceTransactionPreparing
)

// Manager is an interface providing the ability to execute Git hooks.
type Manager interface {
	// PreReceiveHook executes the pre-receive Git hook and any installed custom hooks. stdin
	// must contain all references to be updated and match the format specified in githooks(5).
	PreReceiveHook(ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error

	// PostReceiveHook executes the post-receive Git hook and any installed custom hooks. stdin
	// must contain all references to be updated and match the format specified in githooks(5).
	PostReceiveHook(ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error

	// ReceiveReportHook runs after references have been updated but before the push status is
	// reported to the client. Returning an error aborts the push so that git reports the
	// references as failed. stdin must contain the successfully updated references and match
	// the format specified in githooks(5). It is not implemented yet and always succeeds.
	ReceiveReportHook(ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error

	// UpdateHook executes the update Git hook and any installed custom hooks for the reference
	// `ref` getting updated from `oldValue` to `newValue`.
	UpdateHook(ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error

	// ReferenceTransactionHook executes the reference-transaction Git hook. stdin must contain
	// all references to be updated and match the format specified in githooks(5).
	ReferenceTransactionHook(ctx context.Context, state ReferenceTransactionState, env []string, stdin io.Reader) error
}

// GitLabHookManager is a hook manager containing Git hook business logic. It
// uses the GitLab API to authenticate and track ongoing hook calls.
type GitLabHookManager struct {
	cfg                  config.Cfg
	locator              storage.Locator
	logger               log.Logger
	gitCmdFactory        gitcmd.CommandFactory
	cgroupsManager       cgroups.Manager
	txManager            transaction.Manager
	gitlabClient         gitlab.Client
	postReceiveRegistry  *PostReceiveRegistry
	refUpdatesCounter    *prometheus.CounterVec
	praefectSignalTicker func(time.Duration) helper.Ticker
	// postReceiveWaitTimeout is how long waitForPraefectSignal waits for the
	// Praefect signal before failing the push. It backs praefectSignalTicker and
	// is reported in the timeout error.
	postReceiveWaitTimeout time.Duration
	// pendingTransactions pairs the packed-refs and loose transactions of a
	// deletion, so that the changeset log records the deletion once. A nil value
	// records every committed transaction.
	pendingTransactions *pendingTransactions
	// observer receives the committed reference updates, see
	// ReferenceTransactionHook. A nil observer does nothing.
	observer *eager.Observer
	// policy holds the standalone access policy in force (doc/graft/PLAN12.md). It is never nil:
	// without WithPolicyStore it holds the policy of cfg.
	policy *policystore.Store
	// storageVoter commits reference transactions to the Graft storage core, nil when it is off.
	storageVoter StorageVoter
	// startLandIntentsCompaction starts a compaction of land intents that a prepared
	// transaction asked for, without the hook waiting for it. Nil runs it in a goroutine;
	// tests set it.
	startLandIntentsCompaction func(compact func())
}

// ManagerOption configures a GitLabHookManager built by NewManager.
type ManagerOption func(*GitLabHookManager)

// WithEagerObserver sets the observer that receives the committed reference
// updates. It is called after the changeset log recorded them, see
// ReferenceTransactionHook.
func WithEagerObserver(observer *eager.Observer) ManagerOption {
	return func(m *GitLabHookManager) {
		m.observer = observer
	}
}

// WithPolicyStore sets the store of the standalone access policy that the reference-transaction hook
// decides with, the one the server reloads. Without it the hook decides with the policy of the
// configuration.
func WithPolicyStore(store *policystore.Store) ManagerOption {
	return func(m *GitLabHookManager) {
		m.policy = store
	}
}

// Describe is used to describe Prometheus metrics.
func (m *GitLabHookManager) Describe(descs chan<- *prometheus.Desc) {
	m.refUpdatesCounter.Describe(descs)
}

// Collect is used to collect Prometheus metrics.
func (m *GitLabHookManager) Collect(metrics chan<- prometheus.Metric) {
	m.refUpdatesCounter.Collect(metrics)
}

// Label values for the "update_type" label of the post-receive ref-updates
// counter. They are defined as constants so the pre-initialization here and the
// increment sites in postreceive.go can't drift or typo apart.
const (
	refUpdateCreated = "created"
	refUpdateUpdated = "updated"
	refUpdateDeleted = "deleted"
)

// PostReceiveRegistry provides the PostReceiveRegistry assigned to the Manager. The registry
// coordinates PostReceiveHook execution with Praefect signaling.
func (m *GitLabHookManager) PostReceiveRegistry() *PostReceiveRegistry {
	return m.postReceiveRegistry
}

// NewManager returns a new hook manager
func NewManager(
	cfg config.Cfg,
	locator storage.Locator,
	logger log.Logger,
	gitCmdFactory gitcmd.CommandFactory,
	cgroupsManager cgroups.Manager,
	txManager transaction.Manager,
	gitlabClient gitlab.Client,
	postReceiveRegistry *PostReceiveRegistry,
	opts ...ManagerOption,
) *GitLabHookManager {
	refUpdatesCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "gitaly",
			Subsystem: "hook",
			Name:      "post_receive_pack_ref_updates_total",
			Help:      "Number of reference updates processed by the post-receive hook, by update type",
		},
		[]string{"update_type"},
	)
	// Pre-initialize all known label combinations so the series are present in
	// /metrics from startup, enabling rate() and absent() alerting rules to work
	// correctly before the first push.
	for _, updateType := range []string{refUpdateCreated, refUpdateUpdated, refUpdateDeleted} {
		refUpdatesCounter.WithLabelValues(updateType)
	}

	// Fall back to the built-in default when the config default has not been
	// applied (e.g. a Cfg constructed directly in tests without Sanitize).
	waitTimeout := config.DefaultPostReceiveWaitTimeout
	if configured := cfg.Hooks.PostReceiveWaitTimeout.Duration(); configured > 0 {
		waitTimeout = configured
	}

	m := &GitLabHookManager{
		cfg:                 cfg,
		locator:             locator,
		logger:              logger,
		gitCmdFactory:       gitCmdFactory,
		cgroupsManager:      cgroupsManager,
		txManager:           txManager,
		gitlabClient:        gitlabClient,
		postReceiveRegistry: postReceiveRegistry,
		refUpdatesCounter:   refUpdatesCounter,
		praefectSignalTicker: func(duration time.Duration) helper.Ticker {
			return helper.NewTimerTicker(duration)
		},
		postReceiveWaitTimeout: waitTimeout,
		pendingTransactions:    newPendingTransactions(),
	}

	for _, opt := range opts {
		opt(m)
	}
	if m.policy == nil {
		m.policy = policystore.New(cfg.Graft.Policy)
	}

	return m
}
