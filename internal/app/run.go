package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"media-converter-v2/internal/cache"
	"media-converter-v2/internal/config"
	"media-converter-v2/internal/httpapi"
	"media-converter-v2/internal/job"
	"media-converter-v2/internal/state"
	"media-converter-v2/internal/storage"
)

type UsageError struct {
	Program string
}

func (e *UsageError) Error() string {
	return fmt.Sprintf("usage: %s serve", e.Program)
}

// Main is shared by the compatibility command and the independent v2 binary.
func Main(program string) {
	if err := Run(program, os.Args[1:]); err != nil {
		var usageErr *UsageError
		if errors.As(err, &usageErr) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		slog.Error("media_converter_failed", "error", err)
		os.Exit(1)
	}
}

func Run(program string, args []string) error {
	if len(args) != 1 || args[0] != "serve" {
		return &UsageError{Program: program}
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config_invalid: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	store, err := storage.NewLocalStoreWithModeAndLegacy(cfg.InputRoot, cfg.OutputRoot, cfg.OutputMode, cfg.LegacyArtifactRoot)
	if err != nil {
		return fmt.Errorf("storage_init_failed: %w", err)
	}
	states, err := state.NewStore(cfg.StateRoot)
	if err != nil {
		return fmt.Errorf("state_init_failed: %w", err)
	}
	resultCache, err := cache.NewStore(cfg.CacheRoot)
	if err != nil {
		return fmt.Errorf("cache_init_failed: %w", err)
	}
	if err := os.MkdirAll(cfg.WorkRoot, 0o700); err != nil {
		return fmt.Errorf("workspace_init_failed: %w", err)
	}

	rootReady := make(map[string]bool, 5)
	for name, root := range map[string]string{
		"input": cfg.InputRoot, "output": cfg.OutputRoot, "state": cfg.StateRoot,
		"cache": cfg.CacheRoot, "workspace": cfg.WorkRoot,
	} {
		var readinessErr error
		if name == "output" && cfg.OutputMode == config.OutputModeWebDAV {
			// Do not perform a blocking WebDAV write before the HTTP server starts.
			// The bounded readiness worker verifies writability after startup.
			readinessErr = store.CheckOutputMounted(context.Background())
		} else if name == "output" {
			readinessErr = store.OutputReady(context.Background())
		} else {
			readinessErr = checkWritableDirectory(root)
		}
		rootReady[name] = readinessErr == nil
		if !rootReady[name] {
			logger.Warn("storage_readiness_failed", "root_kind", name, "error", readinessErr)
		}
	}
	artifactStorageReady := rootReady["input"] && rootReady["output"]
	statuses := []config.ToolStatus{
		config.CheckTool(cfg.FFmpegPath, "ffmpeg"),
		config.CheckTool(cfg.FFprobePath, "ffprobe"),
		config.CheckTool(cfg.MagickPath, "imagemagick"),
		config.CheckImageDelegates(cfg.MagickPath),
	}
	ready := true
	readiness := map[string]bool{
		"artifact_storage": artifactStorageReady,
		"workspace":        rootReady["workspace"],
		"state_store":      rootReady["state"],
		"cache_store":      rootReady["cache"],
		"job_manager":      true,
	}
	toolVersions := make(map[string]string, len(statuses))
	for _, status := range statuses {
		toolVersions[status.Name] = status.Version
	}
	if cfg.ToolFingerprint == "" {
		parts := make([]string, 0, len(statuses))
		for _, status := range statuses {
			parts = append(parts, status.Name+"="+status.Version)
		}
		cfg.ToolFingerprint = strings.Join(parts, "|")
	}
	cfg.ToolVersions = toolVersions
	cfg.ToolAvailability = make(map[string]bool, len(statuses))
	cfg.ImageFormats = make(map[string]bool)
	for _, status := range statuses {
		logger.Info("tool_status", "name", status.Name, "available", status.Available, "version", status.Version, "error", status.Error)
		if !status.Available {
			ready = false
		}
		readiness[status.Name] = status.Available
		switch status.Name {
		case "ffmpeg", "ffprobe", "imagemagick":
			cfg.ToolAvailability[status.Name] = status.Available
		case "imagemagick-delegates":
			for format, available := range status.Capabilities {
				cfg.ImageFormats[format] = available
			}
		}
	}
	ready = ready && artifactStorageReady && rootReady["workspace"] && rootReady["state"] && rootReady["cache"]

	manager := job.NewManager(cfg, store, states, resultCache)
	manager.SetLogger(logger)
	defer manager.Stop()
	server := httpapi.NewServer(cfg, manager, store, logger)
	server.SetReadiness(ready, readiness)

	var recoveryMu sync.Mutex
	recovered := false
	recoverIfReady := func() {
		recoveryMu.Lock()
		defer recoveryMu.Unlock()
		if recovered || !server.RefreshReadiness(context.Background()) {
			return
		}
		if err := manager.Cleanup(context.Background()); err != nil {
			logger.Warn("startup_cleanup_failed", "error", err)
			return
		}
		if err := manager.Recover(context.Background()); err != nil {
			logger.Error("job_recovery_failed", "error", err)
			return
		}
		recovered = true
	}
	// Workers can remain idle while a WebDAV mount is absent. This allows the
	// same process to accept work immediately after the mount is restored.
	manager.Start()
	if cfg.OutputMode != config.OutputModeWebDAV || ready {
		recoverIfReady()
	} else {
		logger.Warn("job_recovery_deferred", "reason", "service is not ready")
	}

	httpServer := &http.Server{
		Addr: cfg.ListenAddr, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: cfg.HTTPReadTimeout, WriteTimeout: cfg.HTTPWriteTimeout, IdleTimeout: cfg.HTTPIdleTimeout,
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go func() {
		ticker := time.NewTicker(cfg.JanitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				recoveryMu.Lock()
				readyForCleanup := recovered
				recoveryMu.Unlock()
				if readyForCleanup {
					if err := manager.Cleanup(context.Background()); err != nil {
						logger.Warn("cleanup_failed", "error", err)
					}
				} else {
					recoverIfReady()
				}
			}
		}
	}()
	if cfg.OutputMode == config.OutputModeWebDAV {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
					recoverIfReady()
				}
			}
		}()
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("media_converter_listening", "addr", cfg.ListenAddr, "ready", ready)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serverErr:
		return fmt.Errorf("http_server_failed: %w", err)
	case <-sigCtx.Done():
	}
	server.SetReady(false)
	runCancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	manager.StopWithContext(shutdownCtx)
	logger.Info("media_converter_stopped")
	return nil
}

func checkWritableDirectory(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".readiness-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}
