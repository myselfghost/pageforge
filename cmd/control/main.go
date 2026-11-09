// Command control runs the PageForge control plane.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/api"
	"github.com/pageforge/pageforge/internal/authpolicy"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/config"
	"github.com/pageforge/pageforge/internal/crypto"
	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/gc"
	"github.com/pageforge/pageforge/internal/internalapi"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/observability"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/share"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/version"
	"github.com/pageforge/pageforge/internal/webui"
)

func main() {
	if err := mainCmd(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func mainCmd(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "run":
		return run(args[1:])
	case "-h", "--help", "help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usageText)
	}
}

const usageText = "usage: control run --config <path>"

func usage() error {
	fmt.Fprintln(os.Stderr, usageText)
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "", "path to control-plane YAML config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	var logWriter io.Writer = os.Stdout
	var logCloser io.Closer
	if strings.TrimSpace(cfg.Log.File) != "" {
		fw, err := observability.OpenDailyLogFile(cfg.Log.File, cfg.Log.MaxAgeDays)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		logWriter = fw
		logCloser = fw
	}
	// HTTP 请求日志单独一条通道；未配置时与主日志一样输出到 stdout。
	var accessWriter io.Writer = os.Stdout
	var accessCloser io.Closer
	if strings.TrimSpace(cfg.Log.AccessFile) != "" {
		fw, err := observability.OpenDailyLogFile(cfg.Log.AccessFile, cfg.Log.MaxAgeDays)
		if err != nil {
			return fmt.Errorf("open access log file: %w", err)
		}
		accessWriter = fw
		accessCloser = fw
	}
	defer func() {
		if logCloser != nil {
			_ = logCloser.Close()
		}
		if accessCloser != nil {
			_ = accessCloser.Close()
		}
	}()
	logger := observability.NewLogger(cfg.Log.Level, logWriter)
	accessLogger := observability.NewLogger(cfg.Log.Level, accessWriter)
	// 让所有退回 slog.Default() 的调用点（如 httpx.WriteError、observability
	// 中间件）都写到配置的日志文件，避免绕过 log.file 输出到 stderr。
	slog.SetDefault(logger)
	logger.Info("starting control plane",
		slog.String("listen", cfg.Server.Listen),
		slog.String("internal_listen", cfg.Server.InternalListen),
		slog.String("reserved_prefix", cfg.Server.ReservedPrefix),
		slog.String("database_driver", cfg.Database.Driver),
	)

	cipher, err := crypto.NewFromBase64(cfg.Encryption.MasterKey)
	if err != nil {
		return fmt.Errorf("encryption.master_key: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, store.Dialect(cfg.Database.Driver), cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	applied, err := db.Up(ctx)
	if err != nil {
		return err
	}
	logger.Info("database ready", slog.Int("migrations_applied", len(applied)))

	pools := objectstore.NewPool()
	aliases := alias.NewRepo(db, cipher)
	shares := share.NewRepo(db)
	stores := func(ctx context.Context, p project.Project) (objectstore.ObjectStore, error) {
		a, err := aliases.Get(ctx, p.AliasID)
		if err != nil {
			return nil, err
		}
		if !a.AllowsBucket(p.Bucket) {
			return nil, fmt.Errorf("bucket %q is not allowed for alias %s", p.Bucket, a.ID)
		}
		_, secret, err := aliases.Secret(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		return pools.Get(a.ID, a.CredentialVer, objectstore.Credentials{
			Endpoint:      a.Endpoint,
			Region:        a.Region,
			AccessKey:     a.AccessKey,
			SecretKey:     secret,
			PathStyle:     a.PathStyle,
			TLSSkipVerify: a.TLSSkipVerify,
		})
	}

	idempotencyTTL, err := cfg.Publish.IdempotencyWindow()
	if err != nil {
		return err
	}
	deployService := deploy.NewService(db, deploy.Options{
		Config: deploy.Config{
			ManifestPrefix:  cfg.Storage.ManifestPrefix,
			PackagePrefix:   cfg.Storage.PackagePrefix,
			TempDir:         cfg.Publish.TempDir,
			MaxFiles:        cfg.Publish.MaxFiles,
			MaxFileBytes:    cfg.Publish.MaxFileBytes,
			MaxTotalBytes:   cfg.Publish.MaxTotalBytes,
			MaxPackageBytes: cfg.Publish.MaxPackageBytes,
			IdempotencyTTL:  idempotencyTTL,
			Workers:         cfg.Publish.Workers,
			ShardFiles:      cfg.Publish.ShardFiles,
			ShardBytes:      cfg.Publish.ShardBytes,
		},
		Stores: stores,
		Logger: logger,
	})

	recorder := observability.NewRecorder(200)
	authPolicies := authpolicy.NewRepo(db)
	server := &api.Server{
		Aliases:       aliases,
		Projects:      project.NewRepo(db),
		Bindings:      binding.NewRepo(db, cfg.Server.ReservedPrefix),
		AuthPolicies:  authPolicies,
		Versions:      version.NewRepo(db),
		Shares:        shares,
		Deployments:   deployService,
		AdminToken:    cfg.Server.AdminToken,
		Recorder:      recorder,
		Logger:        logger,
		Stores:        stores,
		PackagePrefix: cfg.Storage.PackagePrefix,
		AliasTester: func(ctx context.Context, a alias.Alias, secret, bucket string) error {
			return testAlias(ctx, pools, a, secret, bucket)
		},
	}

	if cfg.GC.Enabled {
		interval, err := cfg.GC.IntervalDuration()
		if err != nil {
			return err
		}
		grace, err := cfg.GC.OrphanGraceDuration()
		if err != nil {
			return err
		}
		gcService := gc.NewService(db, gc.StoreFactory(stores), gc.Config{
			Interval:      interval,
			OrphanGrace:   grace,
			KeepVersions:  cfg.GC.KeepVersions,
			KeepDays:      cfg.GC.KeepDays,
			PackagePrefix: cfg.Storage.PackagePrefix,
		}, logger)
		go gcService.RunLoop(ctx)
	}

	internalServer := &internalapi.Server{
		Token:        cfg.Server.InternalToken,
		Snapshots:    snapshot.NewGenerator(db, cfg.Server.SnapshotWarnBytes, logger),
		Aliases:      aliases,
		Shares:       shares,
		AuthPolicies: authPolicies,
		Logger:       logger,
	}

	adminMux := http.NewServeMux()
	adminMux.Handle("/api/", server.Handler())
	adminMux.Handle("/healthz", server.Handler())
	adminMux.Handle("/", webui.Handler())

	admin := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           observability.Instrument(accessLogger, recorder, adminMux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	internal := &http.Server{
		Addr:              cfg.Server.InternalListen,
		Handler:           observability.RequestID(accessLogger, internalServer.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 2)
	go serve(admin, "admin", logger, errCh)
	go serve(internal, "internal", logger, errCh)

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			stop()
			shutdown(admin)
			shutdown(internal)
			return err
		}
	}

	shutdown(admin)
	shutdown(internal)
	return nil
}

func testAlias(ctx context.Context, pools *objectstore.Pool, a alias.Alias, secret, bucket string) error {
	objects, err := pools.Get(a.ID, a.CredentialVer, objectstore.Credentials{
		Endpoint:      a.Endpoint,
		Region:        a.Region,
		AccessKey:     a.AccessKey,
		SecretKey:     secret,
		PathStyle:     a.PathStyle,
		TLSSkipVerify: a.TLSSkipVerify,
	})
	if err != nil {
		return err
	}
	probeDone := errors.New("probe done")
	err = objects.List(ctx, bucket, "pageforge-connection-test/", func(objectstore.ObjectInfo) error {
		return probeDone
	})
	if err != nil && !errors.Is(err, probeDone) {
		return err
	}
	return nil
}

func serve(srv *http.Server, name string, logger *slog.Logger, errCh chan<- error) {
	logger.Info("listening", slog.String("server", name), slog.String("addr", srv.Addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("%s server: %w", name, err)
	}
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
