// Command zmail 是 zMail 的入口：加载配置、初始化存储、启动同步引擎与 HTTP 服务。
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zmail/internal/config"
	"zmail/internal/httpapi"
	"zmail/internal/mail"
	"zmail/internal/realtime"
	"zmail/internal/secret"
	"zmail/internal/store"
	"zmail/internal/verify"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "zmail: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVersion = flag.Bool("version", false, "打印版本号后退出")
		genKey      = flag.Bool("genkey", false, "生成 base64 编码的 32 字节密钥后退出")
		checkConfig = flag.Bool("check-config", false, "校验环境变量配置后退出")
		sendTest    = flag.String("verify-code", "", "从给定文本中提取验证码（调试用）")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("zmail %s\n", version)
		return nil
	}
	if *genKey {
		key, err := secret.GenerateKey()
		if err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(key))
		return nil
	}
	if *sendTest != "" {
		fmt.Println(verify.Extract("", *sendTest))
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *checkConfig {
		envFile := cfg.EnvFile
		if envFile == "" {
			envFile = "（未使用 .env，仅读取进程环境变量）"
		}
		fmt.Printf("config ok\n  envFile:   %s\n  listen:    %s\n  publicUrl: %s\n  dbPath:    %s\n  rawDir:    %s\n  logLevel:  %s\n",
			envFile, cfg.ListenAddr, cfg.PublicURL, cfg.DBPath, cfg.RawDir, cfg.LogLevel)
		return nil
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("zMail 启动", "version", version, "listen", cfg.ListenAddr, "publicUrl", cfg.PublicURL, "envFile", cfg.EnvFile)

	if err := os.MkdirAll(cfg.RawDir, 0o750); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	st, err := store.Open(cfg.DBPath, cfg.RawDir)
	if err != nil {
		return err
	}
	defer st.Close()

	box, err := secret.New(cfg.CredentialsKey)
	if err != nil {
		return err
	}

	hub := realtime.NewHub()
	mailMgr := mail.NewManager(cfg, st, box, hub, logger)

	api, err := httpapi.New(httpapi.Deps{
		Cfg:   cfg,
		Store: st,
		Hub:   hub,
		Mail:  mailMgr,
		Log:   logger,
	})
	if err != nil {
		return fmt.Errorf("初始化 HTTP 服务失败: %w", err)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mailMgr.Start(rootCtx)

	go maintenanceLoop(rootCtx, st, logger)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // SSE 需要长连接
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP 监听", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		cancel()
		mailMgr.Stop()
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case sig := <-sigCh:
		logger.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP 关闭超时", "err", err)
	}
	mailMgr.Stop()
	logger.Info("zMail 已停止")
	return nil
}

func newLogger(level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if level == slog.LevelDebug {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// maintenanceLoop 定期清理过期日志、完成任务并检查数据库健康。
func maintenanceLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			nLogs, nJobs, err := st.CleanupRuntimeData(cctx, 30*24*time.Hour, 7*24*time.Hour)
			cancel()
			if err != nil {
				log.Warn("清理历史数据失败", "err", err)
				continue
			}
			if nLogs > 0 || nJobs > 0 {
				log.Info("清理历史数据完成", "syncLogs", nLogs, "jobs", nJobs)
			}
		}
	}
}
