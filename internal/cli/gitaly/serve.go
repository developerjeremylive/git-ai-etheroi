package gitaly

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/feder-cr/git-ai"
	gitalyclient "github.com/feder-cr/git-ai/client"
	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/bootstrap"
	"github.com/feder-cr/git-ai/internal/bootstrap/starter"
	"github.com/feder-cr/git-ai/internal/bundleuri"
	"github.com/feder-cr/git-ai/internal/burdenmonitor"
	"github.com/feder-cr/git-ai/internal/cache"
	"github.com/feder-cr/git-ai/internal/cgroups"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	gitmvcc "github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/config/sentry"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/maintenance"
	"github.com/feder-cr/git-ai/internal/gitaly/server"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mdfile"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/relational"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/checkpoint"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gate"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gc"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	storagemaintenance "github.com/feder-cr/git-ai/internal/graft/corestore/maintenance"
	corestoreopen "github.com/feder-cr/git-ai/internal/graft/corestore/open"
	"github.com/feder-cr/git-ai/internal/graft/corestore/reviewroots"
	corestoresyncer "github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	_ "github.com/feder-cr/git-ai/internal/graft/derived/graph" // registers the graph warmer
	"github.com/feder-cr/git-ai/internal/graft/eager"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	housekeepingmw "github.com/feder-cr/git-ai/internal/grpc/middleware/housekeeping"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/limithandler"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/mvcc"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/helper/env"
	"github.com/feder-cr/git-ai/internal/limiter"
	"github.com/feder-cr/git-ai/internal/loadmonitor"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/streamcache"
	"github.com/feder-cr/git-ai/internal/tempdir"
	"github.com/feder-cr/git-ai/internal/tracing"
	"github.com/feder-cr/git-ai/internal/version"
	"github.com/go-enry/go-license-detector/v4/licensedb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/urfave/cli/v3"
	"gitlab.com/gitlab-org/labkit/fips"
	"gitlab.com/gitlab-org/labkit/monitoring"
	"go.uber.org/automaxprocs/maxprocs"
	"google.golang.org/grpc"

	// Import to register the proxy codec with gRPC.
	_ "github.com/feder-cr/git-ai/proxy"
)

func newServeCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "launch the server daemon",
		UsageText: `gitaly serve <gitaly_config_file>

Example: gitaly serve gitaly.config.toml`,
		Description: "Launch the Gitaly server daemon.",
		Action:      serveAction,
	}
}

// exitWithError logs an error and returns a cli.Exit with the appropriate exit code.
// If a logger is available, it uses the logger's structured logging. Otherwise, it
// returns a plain error message.
func exitWithError(logger log.Logger, msg string, err error) error {
	if logger != nil {
		logger.WithError(err).Error(msg)
		return cli.Exit("", 1)
	}
	return cli.Exit(fmt.Errorf("%s: %w", msg, err), 1)
}

func loadConfig(configPath string) (config.Cfg, error) {
	cfgFile, err := os.Open(configPath)
	if err != nil {
		return config.Cfg{}, err
	}
	defer cfgFile.Close()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return config.Cfg{}, err
	}

	if err := cfg.Validate(); err != nil {
		return config.Cfg{}, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func serveAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() != 1 || cmd.Args().First() == "" {
		cli.ShowSubcommandHelpAndExit(cmd, 2)
	}

	cfg, logger, err := configure(cmd.Args().First())
	if err != nil {
		return exitWithError(logger, "failed to configure Gitaly", err)
	}

	// Flush any buffered log output to disk before the process exits. This is
	// registered early so that it runs after all other deferred cleanup as well
	// as the final shutdown log lines, ensuring we don't lose any logs on
	// shutdown.
	defer func() {
		if err := logger.Flush(); err != nil {
			logger.WithError(err).Warn("failed to flush logs to disk on shutdown")
		}
	}()

	tracingCloser, err := tracing.InitializeTracerProvider(context.Background(), "gitaly")
	if err != nil {
		logger.WithError(err).Error("error initializing distributed tracing")
	}
	defer func() { _ = tracingCloser.Close() }()

	if cfg.Auth.Transitioning && len(cfg.Auth.GetToken()) > 0 {
		logger.Warn("Authentication is enabled but not enforced because transitioning=true. Gitaly will accept unauthenticated requests.")
	}

	logger.WithField("version", version.GetVersion()).Info("Starting Gitaly")
	fips.Check()

	if err := run(cmd, cfg, logger); err != nil {
		return exitWithError(logger, "unclean Gitaly shutdown", err)
	}

	logger.Info("Gitaly shutdown")

	return nil
}

func configure(configPath string) (config.Cfg, log.Logger, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return config.Cfg{}, nil, fmt.Errorf("load config: config_path %q: %w", configPath, err)
	}

	urlSanitizer := log.NewURLSanitizerHook()
	urlSanitizer.AddPossibleGrpcMethod(
		"CreateRepositoryFromURL",
		"FetchRemote",
		"UpdateRemoteMirror",
	)

	errorTruncator := &log.ErrorTruncatorHook{MaxBytes: 1024}

	logger, err := log.Configure(
		log.NewSyncWriter(os.Stdout),
		cfg.Logging.Format,
		cfg.Logging.Level,
		urlSanitizer,
		errorTruncator,
	)
	if err != nil {
		return config.Cfg{}, nil, fmt.Errorf("configuring logger failed: %w", err)
	}

	if err := cfg.ValidateV2(); err != nil {
		logger.Warn(
			fmt.Sprintf(
				"The current configurations will cause Gitaly to fail to start up in future versions. Please run 'gitaly configuration validate < %s' and fix the errors that are printed.",
				configPath,
			),
		)
	}

	if undo, err := maxprocs.Set(maxprocs.Logger(func(s string, i ...interface{}) {
		logger.Info(fmt.Sprintf(s, i...))
	})); err != nil {
		logger.WithError(err).Error("failed to set GOMAXPROCS")
		undo()
	}

	sentry.ConfigureSentry(logger, version.GetVersion(), sentry.Config(cfg.Logging.Sentry))
	cfg.Prometheus.Configure(logger)
	preloadLicenseDatabase(logger)

	return cfg, logger, nil
}

// newGitlabAPIClient selects the client that answers access checks and hook
// callbacks. In Graft standalone mode it is the built-in policy client and
// GitLab is never contacted. Otherwise it is the stub client when hooks are
// bypassed for testing, and the GitLab HTTP client in all other cases.
func newGitlabAPIClient(logger log.Logger, cfg config.Cfg) (gitlab.Client, error) {
	if cfg.Graft.Standalone {
		logger.Info("graft standalone mode: access checks use the built-in policy, GitLab is not contacted")
		return gitlab.NewStandaloneClient(cfg.Graft, logger), nil
	}

	if skipHooks, _ := env.GetBool("GITALY_TESTING_NO_GIT_HOOKS", false); skipHooks {
		logger.Warn("skipping GitLab API client creation since hooks are bypassed via GITALY_TESTING_NO_GIT_HOOKS")
		return gitlab.NewStubClient(), nil
	}

	httpClient, err := gitlab.NewHTTPClient(logger, cfg.Gitlab, cfg.TLS, cfg.Prometheus)
	if err != nil {
		return nil, fmt.Errorf("could not create GitLab API client: %w", err)
	}
	prometheus.MustRegister(httpClient)

	return httpClient, nil
}

// newDerivedPool returns the pool that runs the eager derived-data warmers, sized
// from the [graft.derived] configuration. The pool is registered with registerer, so
// that its graft_derived_warm_* metrics are exported.
func newDerivedPool(logger log.Logger, cfg config.Cfg, repoBuilder eager.RepoBuilder, catfileCache catfile.Cache, registerer prometheus.Registerer) *derived.Pool {
	pool := derived.NewPool(logger, cfg.Graft.Derived.WorkersOrDefault(), cfg.Graft.Derived.QueueSizeOrDefault(),
		eager.WarmEnvOpener(repoBuilder, logger, catfileCache))
	registerer.MustRegister(pool)
	return pool
}

// newEagerOptions returns the options of the eager Observer from the [graft.derived]
// configuration. Eager derived data and the commit-graph write are enabled only by
// their own keys, and neither runs when derived data is disabled. A configuration
// without any [graft] section therefore enqueues nothing and writes no commit-graph
// outside housekeeping. The listings of new commits are bounded like the warmers
// they feed.
func newEagerOptions(cfg config.Cfg) eager.Options {
	return eager.Options{
		Eager:         cfg.Graft.Derived.Eager && !cfg.Graft.Derived.Disabled,
		CommitGraph:   cfg.Graft.Derived.CommitGraph && !cfg.Graft.Derived.Disabled,
		ListWorkers:   cfg.Graft.Derived.WorkersOrDefault(),
		ListQueueSize: cfg.Graft.Derived.QueueSizeOrDefault(),
	}
}

func preloadLicenseDatabase(logger log.Logger) {
	go func() {
		// the first call to `licensedb.Detect` could be too long
		// https://github.com/go-enry/go-license-detector/issues/13
		// this is why we're calling it here to preload license database
		// on server startup to avoid long initialization on gRPC
		// method handling.
		began := time.Now()
		licensedb.Preload()
		logger.WithField("duration_ms", time.Since(began).Milliseconds()).Info("License database preloaded")
	}()
}

func run(appCtx *cli.Command, cfg config.Cfg, logger log.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// shutdownCtx is cancelled once the graceful shutdown period has elapsed. It lets
	// services terminate work that intentionally ignores the per-request context, such
	// as the git-pack-objects processes driven by PackObjectsHookWithSidechannel. Such
	// a handler otherwise never returns while its transfer is stuck, which keeps the
	// server's GracefulStop (and therefore the force Stop that follows it) from
	// completing, wedging shutdown. It is cancelled explicitly below once b.Wait()
	// returns, before the force stop and the deferred cleanups run.
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()

	beganRun := time.Now()

	bootstrapSpan, ctx := tracing.StartSpan(ctx, "gitaly-bootstrap", nil)
	defer bootstrapSpan.End()

	if cfg.RuntimeDir != "" {
		if err := config.PruneOldGitalyProcessDirectories(logger, cfg.RuntimeDir); err != nil {
			return fmt.Errorf("prune runtime directories: %w", err)
		}
	}

	var err error
	cfg, err = config.SetupRuntimeDirectory(cfg, os.Getpid())
	if err != nil {
		return fmt.Errorf("setup runtime directory: %w", err)
	}

	cgroupMgr := cgroups.NewManager(cfg.Cgroups, logger, os.Getpid())

	loadMonitor := loadmonitor.NewLoadMonitor(loadmonitor.Config{
		PollInterval:  time.Second * 10,
		NotifyTimeout: time.Second * 3,
	}, logger, cgroupMgr)
	if err = loadMonitor.Start(ctx); err != nil {
		return fmt.Errorf("starting load monitor: %w", err)
	}
	defer loadMonitor.Stop()
	prometheus.MustRegister(loadMonitor)

	began := time.Now()
	if err := cgroupMgr.Setup(); err != nil {
		return fmt.Errorf("failed setting up cgroups: %w", err)
	}
	if cgroupMgr.Ready() {
		logger.WithField("duration_ms", time.Since(began).Milliseconds()).Info("finished initializing cgroups")
	}

	defer func() {
		if err := os.RemoveAll(cfg.RuntimeDir); err != nil {
			logger.Warn("could not clean up runtime dir")
		}
	}()

	began = time.Now()
	if err := gitaly.UnpackAuxiliaryBinaries(cfg.RuntimeDir, func(string) bool {
		return true
	}); err != nil {
		return fmt.Errorf("unpack auxiliary binaries: %w", err)
	}
	logger.WithField("duration_ms", time.Since(began).Milliseconds()).Info("finished unpacking auxiliary binaries")

	began = time.Now()
	b, err := bootstrap.NewBootstrap(ctx, logger, promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitaly_connections_total",
			Help: "Total number of connections to Gitaly",
		},
		[]string{"type"},
	))
	if err != nil {
		return fmt.Errorf("init bootstrap: %w", err)
	}
	logger.WithField("duration_ms", time.Since(began).Milliseconds()).Info("finished initializing bootstrap")

	began = time.Now()
	gitCmdFactory, cleanup, err := gitcmd.NewExecCommandFactory(cfg, logger, gitcmd.WithCgroupsManager(cgroupMgr))
	if err != nil {
		return fmt.Errorf("creating Git command factory: %w", err)
	}
	defer cleanup()
	logger.WithField("duration_ms", time.Since(began).Milliseconds()).Info("finished initializing command factory")

	logger.WithField("binary_path", gitCmdFactory.GetExecutionEnvironment(ctx).BinaryPath).Info("using Git binary")

	began = time.Now()
	gitVersion, err := gitCmdFactory.GitVersion(ctx)
	if err != nil {
		return fmt.Errorf("git version detection: %w", err)
	}
	logger.WithField("duration_ms", time.Since(began).Milliseconds()).Info("finished detecting git version")

	logger.WithField("version", gitVersion).Info("using Git version")

	registry := backchannel.NewRegistry()
	transactionManager := transaction.NewManager(cfg, logger, registry)
	prometheus.MustRegister(transactionManager)

	locator := config.NewLocator(cfg)

	repoCounter := counter.NewRepositoryCounter(cfg.Storages)
	prometheus.MustRegister(repoCounter)

	prometheus.MustRegister(gitCmdFactory)

	conns := client.NewPool(
		client.WithDialer(client.HealthCheckDialer(
			func(ctx context.Context, address string, opts []grpc.DialOption) (*grpc.ClientConn, error) {
				return client.New(ctx, address, client.WithGrpcOptions(opts))
			},
		)),
		client.WithDialOptions(
			grpc.WithStatsHandler(tracing.NewGRPCClientStatsHandler()),
			client.UnaryInterceptor(),
			client.StreamInterceptor(),
			gitalyclient.WithGitalyDNSResolver(gitalyclient.DefaultDNSResolverBuilderConfig()),
		),
	)
	defer func() {
		_ = conns.Close()
	}()

	catfileCache := catfile.NewCache(cfg)
	defer catfileCache.Stop()
	prometheus.MustRegister(catfileCache)

	localrepoFactory := localrepo.NewFactory(logger, locator, gitCmdFactory, catfileCache)

	diskCache := cache.New(cfg, locator, logger)
	prometheus.MustRegister(diskCache)
	if err := diskCache.StartWalkers(); err != nil {
		return fmt.Errorf("disk cache walkers: %w", err)
	}

	// List of tracking adaptive limits. They will be calibrated by the adaptive calculator
	adaptiveLimits := []limiter.AdaptiveLimiter{}

	perRPCLimits, setupPerRPCConcurrencyLimiters := limithandler.WithConcurrencyLimiters(cfg)
	for _, concurrency := range cfg.Concurrency {
		// Connect adaptive limits to the adaptive calculator
		if concurrency.Adaptive {
			if limit, ok := perRPCLimits.Authenticated[concurrency.RPC]; ok {
				adaptiveLimits = append(adaptiveLimits, limit)
			}
		}
		if concurrency.Global.Adaptive {
			if globalLimit, ok := perRPCLimits.AuthenticatedGlobal[concurrency.RPC]; ok {
				adaptiveLimits = append(adaptiveLimits, globalLimit)
			}
		}
		if concurrency.Unauthenticated.Adaptive {
			if unauthLimit, ok := perRPCLimits.Unauthenticated[concurrency.RPC]; ok {
				adaptiveLimits = append(adaptiveLimits, unauthLimit)
			}
		}
		if concurrency.Unauthenticated.Global.Adaptive {
			if globalLimit, ok := perRPCLimits.UnauthenticatedGlobal[concurrency.RPC]; ok {
				adaptiveLimits = append(adaptiveLimits, globalLimit)
			}
		}
	}
	perRPCLimitHandler := limithandler.New(
		cfg,
		limithandler.LimitConcurrencyByRepo,
		setupPerRPCConcurrencyLimiters,
	)
	prometheus.MustRegister(perRPCLimitHandler)

	packObjectLimit := newPackObjectsAdaptiveLimit("packObjects", cfg.PackObjectsLimiting.ConcurrencyLimits)
	if cfg.PackObjectsLimiting.Adaptive {
		adaptiveLimits = append(adaptiveLimits, packObjectLimit)
	}

	packObjectsMonitor := limiter.NewPackObjectsConcurrencyMonitor(
		cfg.Prometheus.GRPCLatencyBuckets,
		"authenticated",
	)
	packObjectsLimiterAuthenticated := limiter.NewConcurrencyLimiter(
		packObjectLimit,
		cfg.PackObjectsLimiting.QueueMax(),
		cfg.PackObjectsLimiting.MaxQueueWait.Duration(),
		packObjectsMonitor,
	)
	prometheus.MustRegister(packObjectsMonitor)

	var packObjectsLimiter limiter.Limiter = packObjectsLimiterAuthenticated

	if cfg.PackObjectsLimiting.Unauthenticated.IsSet() {
		unauthLimits := cfg.PackObjectsLimiting.Unauthenticated

		packObjectLimitUnauth := newPackObjectsAdaptiveLimit(
			"packObjects-unauthenticated", unauthLimits.ConcurrencyLimits,
		)
		if unauthLimits.Adaptive {
			adaptiveLimits = append(adaptiveLimits, packObjectLimitUnauth)
		}

		packObjectsMonitorUnauth := limiter.NewPackObjectsConcurrencyMonitor(
			cfg.Prometheus.GRPCLatencyBuckets,
			"unauthenticated",
		)
		prometheus.MustRegister(packObjectsMonitorUnauth)

		// Anonymous traffic may additionally be bounded across the node as a whole, which
		// keeps its budget from growing with the number of addresses it arrives from.
		var opts []limiter.Option
		if unauthLimits.Global.IsSet() {
			packObjectLimitGlobal := newPackObjectsAdaptiveLimit(
				"packObjects-unauthenticated-global", unauthLimits.Global,
			)
			if unauthLimits.Global.Adaptive {
				adaptiveLimits = append(adaptiveLimits, packObjectLimitGlobal)
			}

			packObjectsMonitorGlobal := limiter.NewPackObjectsConcurrencyMonitor(
				cfg.Prometheus.GRPCLatencyBuckets,
				"unauthenticated-global",
			)
			prometheus.MustRegister(packObjectsMonitorGlobal)

			opts = append(opts, limiter.WithGlobalLimit(
				packObjectLimitGlobal,
				unauthLimits.Global.MaxQueueSize,
				unauthLimits.Global.MaxQueueWait.Duration(),
				packObjectsMonitorGlobal,
			))
		}

		packObjectsLimiterUnauthenticated := limiter.NewConcurrencyLimiter(
			packObjectLimitUnauth,
			unauthLimits.MaxQueueSize,
			unauthLimits.MaxQueueWait.Duration(),
			packObjectsMonitorUnauth,
			opts...,
		)

		// Use DualLimiter to switch between authenticated and unauthenticated limiters
		packObjectsLimiter = limiter.NewDualLimiter(packObjectsLimiterAuthenticated, packObjectsLimiterUnauthenticated)
	}

	// Enable the adaptive calculator only if there is any limit needed to be adaptive.
	if len(adaptiveLimits) > 0 {
		calcCfg := limiter.Config{
			CalibrationInterval: limiter.DefaultCalibrateFrequency,
			CPUThreshold:        cfg.AdaptiveLimiting.CPUThrottledThreshold,
			MemoryThreshold:     cfg.AdaptiveLimiting.MemoryThreshold,
			PSIConfig:           cfg.AdaptiveLimiting.PSIPressure,
		}
		adaptiveCalculator := limiter.NewAdaptiveCalculator(calcCfg, logger, loadMonitor, adaptiveLimits)
		prometheus.MustRegister(adaptiveCalculator)

		stop, err := adaptiveCalculator.Start(ctx)
		if err != nil {
			logger.WithError(err).Warn("error starting adaptive limiter calculator")
		}
		defer stop()
	}

	mvccCacheFactory := gitmvcc.NewCacheFactory(cfg.RuntimeDir, locator)
	defer func() { _ = mvccCacheFactory.Close(ctx) }()
	prometheus.MustRegister(mvccCacheFactory)

	// The proposals of the land expire in the housekeeping pass too, so that those of idle and
	// deleted branches go (PLAN10 14). proposal_ttl was validated with the configuration.
	proposalTTL, err := cfg.Graft.Land.Resolver.ProposalTTLOrDefault()
	if err != nil {
		return fmt.Errorf("graft land resolver: %w", err)
	}
	housekeepingManager := housekeepingmgr.New(cfg.Prometheus, logger, transactionManager, nil,
		housekeepingmgr.WithMVCCCacheSupport(mvccCacheFactory), housekeepingmgr.WithGraftProposalTTL(proposalTTL))
	prometheus.MustRegister(housekeepingManager)

	housekeepingMiddlewareConfig := housekeepingmw.DefaultMiddlewareConfig()
	housekeepingMiddleware := housekeepingmw.NewHousekeepingMiddleware(logger, protoregistry.GitalyProtoPreregistered, localrepoFactory, housekeepingManager, housekeepingMiddlewareConfig)
	// Stop cancels any in-flight housekeeping workers before waiting for them. The workers are
	// detached from their request context, so without cancellation they keep optimizing the
	// repository until completion and can block shutdown for minutes after the grace period
	// expired and the servers have been force-stopped.
	defer housekeepingMiddleware.Stop()

	bm := burdenmonitor.New(logger)
	bm.StartPoller(ctx)

	if cfg.LoadShedder.Enabled {
		loadShedder, err := burdenmonitor.NewLoadShedder(
			burdenmonitor.LoadShedderConfig{PSI: cfg.AdaptiveLimiting.PSIPressure},
			logger, loadMonitor, bm,
		)
		if err != nil {
			return fmt.Errorf("creating load shedder: %w", err)
		}
		loadShedder.Start(ctx)
	}

	mvccMiddleware := mvcc.NewMvccInterceptor(mvccCacheFactory, logger, protoregistry.GitalyProtoPreregistered, locator)

	gitalyServerFactory := server.NewGitalyServerFactory(
		cfg,
		logger,
		registry,
		diskCache,
		[]*limithandler.LimiterMiddleware{perRPCLimitHandler},
		housekeepingMiddleware,
		server.TransactionMiddleware{},
		mvccMiddleware,
	)

	gitlabClient, err := newGitlabAPIClient(logger, cfg)
	if err != nil {
		return err
	}

	// Eager derived data and commit-graph writes run after committed reference
	// updates, see internal/graft/eager. The warmers run in one bounded pool.
	repoBuilder := func(repo storage.Repository) *localrepo.Repo {
		return localrepo.New(logger, locator, gitCmdFactory, catfileCache, repo)
	}

	derivedPool := newDerivedPool(logger, cfg, repoBuilder, catfileCache, prometheus.DefaultRegisterer)
	go derivedPool.Run(ctx)
	defer derivedPool.Close()

	eagerObserver := eager.NewObserver(ctx, logger,
		newEagerOptions(cfg),
		eager.Deps{
			Enqueuer:         derivedPool,
			ListNewCommits:   eager.NewCommitsLister(repoBuilder),
			WriteCommitGraph: eager.CommitGraphWriterFor(repoBuilder, housekeepingManager), // serialized with OptimizeRepository
		},
	)
	defer eagerObserver.Close()
	prometheus.MustRegister(eagerObserver)

	// The Graft storage core keeps the repository state in a database plus object storage
	// (doc/graft/STORAGE.md). It is nil when [graft.storage] mode is off.
	storageCore, err := corestoreopen.Open(ctx, cfg.Graft.Storage)
	if err != nil {
		return fmt.Errorf("graft storage core: %w", err)
	}
	defer func() { _ = storageCore.Close() }()
	// The review roots are written and swept in every mode, also without the
	// storage core.
	prometheus.MustRegister(reviewroots.DefaultSweeper)

	hookManagerOpts := []hook.ManagerOption{hook.WithEagerObserver(eagerObserver)}
	var (
		storageSyncer corestore.Freshener
		storageGate   *gate.Gate
	)
	if storageCore.Enabled() {
		if storageCore.Authoritative() {
			if err := openLocalReferenceWrites(storageCore, locator, logger); err != nil {
				return err
			}
		}

		syncer := corestoresyncer.New(storageCore, logger, locator, localrepoFactory, gitCmdFactory)
		prometheus.MustRegister(syncer)
		storageSyncer = syncer

		storageVoter := voter.New(storageCore, logger, localrepoFactory, locator, syncer)
		prometheus.MustRegister(storageVoter)
		hookManagerOpts = append(hookManagerOpts, hook.WithStorageVoter(storageVoter))

		if storageCore.Authoritative() {
			// Before any listener opens: a repository whose references may be torn
			// by a kernel restart is moved aside, so nothing reads it before it is
			// hydrated again (doc/graft/PLAN7.md, LOCAL REFERENCE WRITES RULE).
			storageNames := make([]string, 0, len(cfg.Storages))
			for _, s := range cfg.Storages {
				storageNames = append(storageNames, s.Name)
			}
			// The build directories of hydrations of a process that died are
			// removed; those of a process that runs (the old one of a graceful
			// upgrade) hold their lock and are kept. A failure is logged and
			// counted: it never stops the server.
			syncer.CleanBuilds(ctx, storageNames)
			stopLocalReferences, err := syncer.StartLocalReferences(ctx, storageNames)
			if err != nil {
				return err
			}
			// Deferred here, so it runs after the servers stopped: a last durable step
			// settles every repository whose writes ended.
			defer stopLocalReferences()
			// Runs before it (deferred later): the seeds in the background end
			// before the last durable step.
			defer syncer.StopSeeds()

			if cfg.Graft.Storage.MaintenanceAuto() {
				stopMaintenance, err := startStorageMaintenance(ctx, cfg, storageCore, syncer, locator, localrepoFactory, logger, storageNames)
				if err != nil {
					return err
				}
				defer stopMaintenance()
			}

			storageGate = gate.New(storageCore, syncer, logger, locator, localrepoFactory, protoregistry.GitalyProtoPreregistered)
		}

		logger.WithField("mode", string(storageCore.Mode)).WithField("node_id", storageCore.NodeID).Info("graft storage core enabled")
	}

	postReceiveRegistry := hook.NewPostReceiveRegistry()
	hookManager := hook.NewManager(
		cfg,
		locator,
		logger,
		gitCmdFactory,
		cgroupMgr,
		transactionManager,
		gitlabClient,
		postReceiveRegistry,
		hookManagerOpts...,
	)
	prometheus.MustRegister(hookManager)

	updaterWithHooks := updateref.NewUpdaterWithHooks(cfg, logger, locator, hookManager, gitCmdFactory, catfileCache)

	packObjectStreamCache := streamcache.New(cfg.PackObjectsCache, logger)
	archiveStreamCache := streamcache.New(cfg.ArchiveCache, logger)

	var backupSink *backup.Sink
	var backupLocator backup.Locator
	if cfg.Backup.GoCloudURL != "" {
		var err error
		backupSink, err = backup.ResolveSink(ctx, cfg.Backup.GoCloudURL, backup.WithBufferSize(cfg.Backup.BufferSize))
		if err != nil {
			return fmt.Errorf("resolve backup sink: %w", err)
		}
		backupLocator = backup.NewLocator(backupSink)
	}

	var poolMetadataStore relational.PoolStore
	if cfg.PoolMetadata.DatabasePath != "" {
		var err error
		poolMetadataStore, err = relational.NewSQLitePoolStore(cfg.PoolMetadata.DatabasePath)
		if err != nil {
			return fmt.Errorf("create pool metadata store: %w", err)
		}
		defer func() { _ = poolMetadataStore.Close() }()
		logger.Info(fmt.Sprintf("pool metadata store configured: %s", cfg.PoolMetadata.DatabasePath))
	}

	objectPoolStateManager := relational.NewObjectPoolStateManager(poolMetadataStore)

	var bundleURIManager *bundleuri.GenerationManager
	if cfg.BundleURI.GoCloudURL != "" {
		bundleURISink, err := bundleuri.NewSink(ctx, cfg.BundleURI.GoCloudURL)
		if err != nil {
			return fmt.Errorf("create bundle-URI sink: %w", err)
		}

		// The manager created here merely to have a non-nil manager in order to use
		// the SignedURL() method on it. It is not used, yet, to generate bundles
		// based on this configuration.
		// Further tests and analysis would be required to come up with the
		// appropriate configuration. This will be done once we are ready to use this manager
		// to generate bundles.
		maxBundleAge := time.Hour * 24
		interval := time.Minute
		maxConcurrent := 5
		threshold := 5
		bundleGenerationStrategy, err := bundleuri.NewOccurrenceStrategy(logger, threshold, interval, maxConcurrent, maxBundleAge)
		if err != nil {
			return fmt.Errorf("error creating bundle generation strategy: %w", err)
		}

		prometheus.MustRegister(bundleGenerationStrategy)
		stop := bundleGenerationStrategy.Start(ctx)
		defer stop()

		bundleURIManager, err = bundleuri.NewGenerationManager(ctx, bundleURISink, logger, nil, bundleGenerationStrategy)
		if err != nil {
			return fmt.Errorf("error creating bundle manager: %w", err)
		}
		logger.Info(fmt.Sprintf("bundle-uri bucket configured: %s", cfg.BundleURI.GoCloudURL))
		prometheus.MustRegister(bundleURIManager)
	}

	// The land batching lanes are shared by the operations servers of every listener, so that the
	// requests for one branch are batched in one lane. They are closed after the servers stopped,
	// once the shutdown context has ended the batches that are still running.
	landLaneCfg, err := landlane.ConfigFromGraft(cfg.Graft.Land.Batch)
	if err != nil {
		return fmt.Errorf("graft land batching: %w", err)
	}
	landLanes := landlane.NewShared(landLaneCfg)
	prometheus.MustRegister(landLanes)
	defer landLanes.Close()

	// The conflict resolver is built once and shared by the operations servers of every
	// listener, so that its check slots bound the process (check_concurrency) and a resolver
	// that cannot be built is one error, which makes every land that needs it fail closed.
	graftResolver := resolver.NewShared(cfg.Graft.Land.Resolver)
	if graftResolver.Err != nil {
		logger.WithError(graftResolver.Err).Error("graft: the conflict resolver of land cannot be built: lands that need it fail")
	}

	for _, c := range []starter.Config{
		{Name: starter.Unix, Addr: cfg.SocketPath, HandoverOnUpgrade: true},
		{Name: starter.Unix, Addr: cfg.InternalSocketPath(), HandoverOnUpgrade: false},
		{Name: starter.TCP, Addr: cfg.ListenAddr, HandoverOnUpgrade: true},
		{Name: starter.TLS, Addr: cfg.TLSListenAddr, HandoverOnUpgrade: true},
	} {
		if c.Addr == "" {
			continue
		}

		var srv *grpc.Server
		opts := []server.Option{
			server.WithUnaryInterceptor(bm.UnaryInterceptor()),
			server.WithStreamInterceptor(bm.StreamInterceptor()),
		}
		if storageGate != nil {
			opts = append(opts,
				server.WithUnaryInterceptor(storageGate.UnaryInterceptor()),
				server.WithStreamInterceptor(storageGate.StreamInterceptor()),
			)
		}

		if c.HandoverOnUpgrade {
			srv, err = gitalyServerFactory.CreateExternal(c.IsSecure(), opts...)
			if err != nil {
				return fmt.Errorf("create external gRPC server: %w", err)
			}
		} else {
			srv, err = gitalyServerFactory.CreateInternal(opts...)
			if err != nil {
				return fmt.Errorf("create internal gRPC server: %w", err)
			}
		}

		setup.RegisterAll(srv, &service.Dependencies{
			Logger:                 logger,
			Cfg:                    cfg,
			GitalyHookManager:      hookManager,
			TransactionManager:     transactionManager,
			StorageLocator:         locator,
			ClientPool:             conns,
			GitCmdFactory:          gitCmdFactory,
			CgroupsManager:         cgroupMgr,
			CatfileCache:           catfileCache,
			DiskCache:              diskCache,
			PackObjectsCache:       packObjectStreamCache,
			PackObjectsLimiter:     packObjectsLimiter,
			RepositoryCounter:      repoCounter,
			UpdaterWithHooks:       updaterWithHooks,
			HousekeepingManager:    housekeepingManager,
			BackupSink:             backupSink,
			BackupLocator:          backupLocator,
			LocalRepositoryFactory: localrepoFactory,
			BundleURIManager:       bundleURIManager,
			ArchiveCache:           archiveStreamCache,
			PoolMetadataStore:      poolMetadataStore,
			GitlabClient:           gitlabClient,
			ObjectPoolStateManager: objectPoolStateManager,
			PostReceiveRegistry:    postReceiveRegistry,
			GraftLandLanes:         landLanes,
			GraftResolver:          graftResolver,
			GraftStorage:           storageCore,
			GraftStorageSyncer:     storageSyncer,
			ShutdownContext:        shutdownCtx,
		})
		b.RegisterStarter(starter.New(c, srv, logger))
	}

	if addr := cfg.PrometheusListenAddr; addr != "" {
		b.RegisterStarter(func(listen bootstrap.ListenFunc, _ chan<- error, _ *prometheus.CounterVec) error {
			l, err := listen("tcp", addr)
			if err != nil {
				return err
			}

			logger.WithField("address", addr).Info("starting prometheus listener")

			go func() {
				opts := []monitoring.Option{
					monitoring.WithListener(l),
					monitoring.WithBuildExtraLabels(
						map[string]string{"git_version": gitVersion},
					),
				}

				if buildInfo, ok := debug.ReadBuildInfo(); ok {
					opts = append(opts, monitoring.WithGoBuildInformation(buildInfo))
				}

				if err := monitoring.Start(opts...); err != nil {
					logger.WithError(err).Error("Unable to serve prometheus")
				}
			}()

			return nil
		})
	}

	for _, shard := range cfg.Storages {
		if err := mdfile.WriteMetadataFile(ctx, shard.Path); err != nil {
			// TODO should this be a return? https://gitlab.com/gitlab-org/gitaly/issues/1893
			logger.WithError(err).Error("Unable to write gitaly metadata file")
		}
	}

	// When cgroups are configured, we create a directory structure each
	// time a gitaly process is spawned. Look through the hierarchy root
	// to find any cgroup directories that belong to old gitaly processes
	// and remove them.
	cgroups.StartPruningOldCgroups(cfg.Cgroups, logger)
	repoCounter.StartCountingRepositories(ctx, locator, logger)
	tempdir.StartCleaning(logger, locator, cfg.Storages, time.Hour)

	if err := b.Start(); err != nil {
		return fmt.Errorf("unable to start the bootstrap: %w", err)
	}
	bootstrapSpan.End()
	// There are a few goroutines running async tasks that may still be in progress (i.e. preloading the license
	// database), but this is a close enough indication of startup latency.
	logger.WithField("duration_ms", time.Since(beganRun).Milliseconds()).Info("Started Gitaly")

	if !cfg.DailyMaintenance.IsDisabled() {
		shutdownWorkers, err := maintenance.StartWorkers(
			ctx,
			logger,
			maintenance.DailyOptimizationWorker(cfg, dailyOptimizer(
				func(logger log.Logger, repo storage.Repository) *localrepo.Repo {
					return localrepo.New(logger, locator, gitCmdFactory, catfileCache, repo)
				},
				func(ctx context.Context, logger log.Logger, local *localrepo.Repo, repo storage.Repository) error {
					return housekeepReviewRoots(ctx, logger, storageCore, storageSyncer, local, repo)
				},
				func(ctx context.Context, local *localrepo.Repo) error {
					return housekeepingManager.OptimizeRepository(ctx, local)
				},
			)),
		)
		if err != nil {
			return fmt.Errorf("initialize auxiliary workers: %w", err)
		}
		defer shutdownWorkers()
	}

	gracefulStopTicker := helper.NewTimerTicker(cfg.GracefulRestartTimeout.Duration())
	defer gracefulStopTicker.Stop()

	logger.WithField("duration_s", cfg.GracefulRestartTimeout.Duration().String()).Info("Gitaly graceful stop timeout duration")
	waitErr := b.Wait(gracefulStopTicker, gitalyServerFactory.GracefulStop)
	logger.WithField("error_message", waitErr).Info("Wait has returned with error")

	// The grace period has elapsed. Cancel the shutdown context so that services
	// terminate work that intentionally ignores the per-request context (such as
	// git-pack-objects driven by PackObjectsHookWithSidechannel). Such a handler
	// otherwise keeps running, so the in-flight GracefulStop above never finishes
	// its handlersWG wait; because it holds the gRPC server mutex while waiting,
	// the force Stop below cannot even acquire the mutex, and shutdown wedges until
	// the handler eventually returns on its own.
	cancelShutdown()

	// Arm a watchdog that dumps the stacks of any goroutines still holding up
	// shutdown, so a wedged shutdown is observable rather than a silent
	// multi-minute hang. It only logs; it does not change shutdown behavior. It is
	// intentionally not disarmed: a clean shutdown exits before the timeout, which
	// kills the watchdog goroutine. See internal/bootstrap/shutdown_watchdog.go.
	bootstrap.StartShutdownWatchdog(logger, bootstrap.ShutdownWatchdogTimeout(cfg.GracefulRestartTimeout.Duration()))

	// Always force-stop servers immediately after Wait returns. If the grace
	// period expired or a force shutdown was requested, this ensures in-flight
	// connections are killed before deferred functions run, preventing them
	// from blocking on operations that will never complete. If GracefulStop
	// already completed cleanly, this is a no-op.
	gitalyServerFactory.Stop()

	return waitErr
}

// dailyOptimizer is the job of the daily maintenance for one repository: the backstop of the
// review roots, in every mode, then the housekeeping that prunes unreferenced objects
// (doc/graft/PLAN7.md, REVIEW ROOTS RULE). The commits of an open review written without its
// keep-around references are referenced before a prune could take them, so a failed backstop
// skips the housekeeping of the repository.
func dailyOptimizer(
	newLocal func(log.Logger, storage.Repository) *localrepo.Repo,
	backstop func(context.Context, log.Logger, *localrepo.Repo, storage.Repository) error,
	optimize func(context.Context, *localrepo.Repo) error,
) maintenance.Optimizer {
	return maintenance.OptimizerFunc(func(ctx context.Context, logger log.Logger, repo storage.Repository) error {
		local := newLocal(logger, repo)
		if err := backstop(ctx, logger, local, repo); err != nil {
			return err
		}
		return optimize(ctx, local)
	})
}

// housekeepReviewRoots runs the backstop of the review roots before the daily
// housekeeping of a repository: it writes the keep-around references that
// open reviews lack and deletes those no open review uses (in authoritative
// mode after catching the repository up). Open reviews whose commits are lost
// are logged with their IDs and commits; the counts are metrics of
// reviewroots.DefaultSweeper.
func housekeepReviewRoots(ctx context.Context, logger log.Logger, core *corestore.Core, fresh corestore.Freshener, local *localrepo.Repo, repo storage.Repository) error {
	backfill, sweep, err := reviewroots.DefaultSweeper.Housekeep(ctx, core, fresh, local, repo)
	for _, missing := range backfill.Missing {
		logger.WithFields(log.Fields{
			"review_id": missing.ReviewID,
			"commit_id": missing.Commit,
		}).Warn("review roots: a commit of an open review is not in the repository; the review can only be closed")
	}
	if backfill.Protected > 0 || sweep.Deleted > 0 {
		logger.WithFields(log.Fields{
			"backfilled": backfill.Protected,
			"deleted":    sweep.Deleted,
		}).Info("review roots: housekeeping")
	}
	if err != nil {
		return fmt.Errorf("review roots: %w", err)
	}
	return nil
}

// openLocalReferenceWrites sets up the admission and the deferred durability
// of the local reference writes of the storage core in authoritative mode
// (doc/graft/PLAN7.md, LOCAL REFERENCE WRITES RULE). Without the kernel's boot
// identity (not Linux) the durability is not deferred: every reference write
// is made durable by git, as in the other modes.
func openLocalReferenceWrites(core *corestore.Core, locator storage.Locator, logger log.Logger) error {
	lockTimeout := gitcmd.ReftableLockTimeoutMilliseconds * time.Millisecond
	core.RefAdmission = localrefs.NewAdmission(lockTimeout, lockTimeout)
	prometheus.MustRegister(core.RefAdmission)

	durability, err := localrefs.NewDurability(locator, localrefs.DurableStepIdle,
		localrefs.WithWatermark(gitops.WatermarkPath))
	if err != nil {
		return fmt.Errorf("graft storage core: %w", err)
	}
	if durability == nil {
		logger.Info("graft storage core: the kernel exposes no boot identity, every reference write is made durable by git")
		return nil
	}
	core.RefDurability = durability
	prometheus.MustRegister(durability)
	return nil
}

// startStorageMaintenance starts the background checkpoints and garbage
// collection of the storage core ([graft.storage] checkpoints, on by default,
// and gc, opt-in; authoritative mode only; doc/graft/PLAN7.md, "Background
// maintenance").
func startStorageMaintenance(ctx context.Context, cfg config.Cfg, core *corestore.Core, fresh corestore.Freshener, locator storage.Locator, repoFactory localrepo.Factory, logger log.Logger, storageNames []string) (func(), error) {
	interval, err := cfg.Graft.Storage.MaintenanceIntervalOrDefault()
	if err != nil {
		return nil, err
	}
	leaseTTL, err := cfg.Graft.Storage.LeaseTTLOrDefault()
	if err != nil {
		return nil, err
	}

	runner := storagemaintenance.New(core,
		checkpoint.New(core, logger, locator, repoFactory, fresh),
		gc.New(core, logger),
		fresh, logger,
		storagemaintenance.Config{
			Interval:             interval,
			Checkpoints:          cfg.Graft.Storage.CheckpointsAuto(),
			CheckpointChangesets: cfg.Graft.Storage.CheckpointChangesetsOrDefault(),
			CheckpointPackBytes:  cfg.Graft.Storage.CheckpointPackBytesOrDefault(),
			Collect:              cfg.Graft.Storage.GCAuto(),
			LeaseTTL:             leaseTTL,
		},
		storageNames,
	)
	prometheus.MustRegister(runner)
	logger.WithField("interval", interval.String()).
		WithField("checkpoints", cfg.Graft.Storage.CheckpointsAuto()).
		WithField("gc", cfg.Graft.Storage.GCAuto()).
		Info("graft storage core: background maintenance enabled")
	return runner.Start(ctx), nil
}

// newPackObjectsAdaptiveLimit builds the adaptive limit backing a pack objects limiter. The limit
// only moves when the configuration asks for an adaptive one, and is otherwise pinned to the
// configured concurrency.
func newPackObjectsAdaptiveLimit(name string, limits config.ConcurrencyLimits) *limiter.AdaptiveLimit {
	if !limits.Adaptive {
		return limiter.NewAdaptiveLimit(name, limiter.AdaptiveSetting{
			Initial: limits.Concurrency(),
		})
	}

	return limiter.NewAdaptiveLimit(name, limiter.AdaptiveSetting{
		Initial:       limits.InitialLimit,
		Max:           limits.MaxLimit,
		Min:           limits.MinLimit,
		BackoffFactor: limiter.DefaultBackoffFactor,
	})
}
