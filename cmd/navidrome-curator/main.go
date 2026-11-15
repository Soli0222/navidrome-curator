// navidrome-curator は、大きな音楽の置き場から選んだアルバムだけを、音楽サーバーのライブラリにコピーする。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/config"
	"github.com/Soli0222/navidrome-curator/internal/cover"
	"github.com/Soli0222/navidrome-curator/internal/index"
	"github.com/Soli0222/navidrome-curator/internal/jobs"
	"github.com/Soli0222/navidrome-curator/internal/library"
	"github.com/Soli0222/navidrome-curator/internal/metrics"
	"github.com/Soli0222/navidrome-curator/internal/notify"
	"github.com/Soli0222/navidrome-curator/internal/scan"
	"github.com/Soli0222/navidrome-curator/internal/server"
	"github.com/Soli0222/navidrome-curator/internal/trash"
	"github.com/Soli0222/navidrome-curator/web"
)

// version は release のときに -ldflags で埋め込む。
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	defaultPath := os.Getenv(config.EnvConfigPath)
	if defaultPath == "" {
		defaultPath = "/etc/navidrome-curator/config.yaml"
	}
	configPath := flag.String("config", defaultPath, "path to the config file (env "+config.EnvConfigPath+")")
	checkOnly := flag.Bool("check-config", false, "validate the config and the filesystem, then exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := cfg.LoadSecrets(); err != nil {
		return err
	}
	if err := cfg.Prepare(); err != nil {
		return fmt.Errorf("config check failed:\n%w", err)
	}
	if *checkOnly {
		log.Info("config ok", "path", *configPath)
		return nil
	}
	if cfg.Auth.Mode == config.AuthNone {
		log.Warn("auth.mode is none: anyone who can reach this server can change the libraries; use it only inside a trusted network")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rules, err := scan.NewRules(cfg.AudioExtensions, cfg.Exclude, *cfg.ExcludeDotDirectories)
	if err != nil {
		return err
	}
	m := metrics.New()
	dirMode := cfg.Copy.DirMode.Mode()

	tr, err := trash.New(cfg.TrashDir(), dirMode)
	if err != nil {
		return fmt.Errorf("trash: %w", err)
	}

	covers, err := cover.New(rules, filepath.Join(cfg.WorkDir, "cache", "covers"), dirMode, log)
	if err != nil {
		return fmt.Errorf("cover cache: %w", err)
	}

	var srcs []index.Source
	srcPaths := map[string]string{}
	for _, s := range cfg.Sources {
		srcs = append(srcs, index.Source{Name: s.Name, Path: s.Path})
		srcPaths[s.Name] = s.Path
		m.SourceUp.WithLabelValues(s.Name).Set(0)
	}
	idx := index.New(srcs, rules, log, index.Hooks{OnBuilt: func(source string, ok bool, at time.Time) {
		if ok {
			m.SourceUp.WithLabelValues(source).Set(1)
			m.IndexLastSuccess.WithLabelValues(source).Set(float64(at.Unix()))
		} else {
			m.SourceUp.WithLabelValues(source).Set(0)
		}
	}})

	var libs []library.Library
	for _, l := range cfg.Libraries {
		libs = append(libs, library.Library{Name: l.Name, Path: l.Path})
	}
	lm := library.New(libs, rules, tr, dirMode, log)
	lm.OnScanned(func(sums map[string]*library.Summary) {
		for name, s := range sums {
			if s.Err != nil {
				continue
			}
			m.LibraryBytes.WithLabelValues(name).Set(float64(s.Bytes))
			m.LibraryAlbums.WithLabelValues(name).Set(float64(len(s.Albums)))
		}
		m.TrashBytes.Set(float64(tr.Bytes()))
		if u, err := scan.Statfs(cfg.LibraryRoot); err == nil {
			m.FilesystemFree.Set(float64(u.Free))
		}
	})

	store, err := jobs.OpenStore(cfg.StatePath())
	if err != nil {
		// 履歴が読めなくてもコピーと削除はできる。メモリに置いて続ける。
		log.Error("cannot open the state db; keeping job history in memory only", "path", cfg.StatePath(), "error", err)
		store, err = jobs.OpenStore(":memory:")
		if err != nil {
			return err
		}
	}
	defer func() { _ = store.Close() }()

	notifier := notify.New(ctx, cfg.Notify.Webhook.URL, cfg.Notify.Webhook.Debounce.D(), cfg.Notify.Webhook.Timeout.D(), log,
		func() { m.NotifyFailures.Inc() })

	jm := jobs.New(jobs.Options{
		Sources:         srcPaths,
		LibraryRoot:     cfg.LibraryRoot,
		StagingDir:      cfg.StagingDir(),
		FreeSpaceMargin: int64(cfg.Copy.FreeSpaceMargin),
		FileMode:        cfg.Copy.FileMode.Mode(),
		DirMode:         dirMode,
		Concurrency:     cfg.Copy.Concurrency,
		HistoryLimit:    cfg.Jobs.HistoryLimit,
		TrashBytes:      tr.Bytes,
	}, rules, lm, store, log, jobs.Hooks{
		OnFinished: func(j jobs.Job, _ time.Duration) {
			m.JobsTotal.WithLabelValues(j.Status).Inc()
			if j.Status == jobs.StatusCompleted || j.Status == jobs.StatusFailed {
				notifier.Add(notify.Event{
					JobID: j.ID, Source: j.Source, Path: j.Rel, Library: j.Library, Result: j.Status,
					Resync: j.Resync, ErrorCode: j.ErrorCode, FinishedAt: j.FinishedAt,
				})
			}
		},
		OnCopiedBytes: func(n int64) { m.CopiedBytesTotal.Add(float64(n)) },
		OnQueueChange: func(n int) { m.QueueLength.Set(float64(n)) },
	})
	if err := jm.Start(ctx); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}

	go idx.Run(ctx, cfg.Index.RefreshInterval.D())
	go lm.Run(ctx, 10*time.Minute)
	go covers.RunPrune(ctx, 90*24*time.Hour, 24*time.Hour)
	go tr.RunPurge(ctx, cfg.Trash.Retention.D(), time.Hour,
		func(err error) { log.Error("trash purge failed", "error", err) },
		func(n int) {
			log.Info("expired trash entries removed", "entries", n)
			m.TrashBytes.Set(float64(tr.Bytes()))
		})

	srv := &server.Server{
		Config: cfg, Rules: rules, Covers: covers, Index: idx, Libs: lm, Jobs: jm, Trash: tr, Metrics: m, Log: log,
		Version: version, Static: web.FS(), BaseContext: ctx,
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", version, "auth", cfg.Auth.Mode)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	jm.Wait()
	notifier.Flush()
	return nil
}
