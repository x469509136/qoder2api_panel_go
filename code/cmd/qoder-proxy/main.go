// Command qoder-proxy runs the Qoder gateway.
//
// It exposes an OpenAI-compatible and an Anthropic-compatible HTTP surface in
// front of the Qoder CN/INTL upstream, plus a single-file web panel.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/fingerprint"
	"github.com/xzs/bridge_qoder/internal/logx"
	"github.com/xzs/bridge_qoder/internal/server"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, server.ErrShowVersion) {
			fmt.Println("qoder-proxy " + server.Version)
			return
		}
		if errors.Is(err, server.ErrShowHelp) {
			fmt.Println(server.Usage)
			return
		}
		fmt.Fprintln(os.Stderr, "[ERROR] "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := server.LoadConfig(os.Args[1:])
	if err != nil {
		return err
	}
	// A malformed VPC endpoint is a startup error: silently falling back to the
	// public host would send enterprise credentials to the wrong place.
	if accounts.VPCConfigError != "" {
		return fmt.Errorf("invalid QD_CN_VPC_ENDPOINT: %s\n"+
			"        expected an instance name (e.g. acme) or one of\n"+
			"        acme.vpc.qoder.com.cn / acme-openapi.vpc.qoder.com.cn /\n"+
			"        acme-gateway.vpc.qoder.com.cn", accounts.VPCConfigError)
	}
	fingerprint.SetLegacy(envTruthy("QD_DEVICE_ID_LEGACY"))

	if err := os.MkdirAll(cfg.AccountsDir, 0o755); err != nil {
		return fmt.Errorf("accounts dir: %w", err)
	}
	if err := os.MkdirAll(cfg.UsageDir, 0o755); err != nil {
		return fmt.Errorf("usage dir: %w", err)
	}

	logger := logx.New(logx.Options{Dir: logsDir(cfg), FileName: "qoder-proxy.log"})
	defer logger.Close()

	// A second instance on the same port is nearly impossible to diagnose: two
	// listeners share the socket and connections are split silently.
	if alive, who := probeExisting(cfg); alive {
		fmt.Printf("\n  [已有一个反代在 %d 端口运行，无需重复启动] %s\n", cfg.Port, who)
		return nil
	}

	srv, err := server.New(cfg, logger)
	if err != nil {
		return err
	}
	srv.StartBackground()

	if cfg.PIDFile != "" {
		if err := os.WriteFile(cfg.PIDFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
			logger.Warn("system", "pid file write failed (%s): %v", cfg.PIDFile, err)
		} else {
			defer func() { _ = os.Remove(cfg.PIDFile) }()
		}
	}

	logger.Info("system", "qoder-proxy %s starting", cfg.Version)
	logger.Info("system", "listening  : http://%s:%d/v1", cfg.Host, cfg.Port)
	logger.Info("system", "dashboard  : http://%s:%d/", cfg.Host, cfg.Port)
	logger.Info("system", "store      : %s", cfg.AccountsDir)
	logger.Info("system", "realm      : %s", cfg.DefaultRealm)
	if cfg.LAN {
		logger.Warn("system", "LAN mode: every device on this network can reach the API")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	serveErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		serveErr <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-sig:
		logger.Info("system", "shutdown signal received")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("system", "shutdown: %v", err)
	}
	wg.Wait()
	logger.Info("system", "stopped")
	return nil
}

// logsDir picks where the gateway log lives.
func logsDir(cfg *server.Config) string {
	if dir := strings.TrimSpace(os.Getenv("QD_LOG_DIR")); dir != "" {
		return dir
	}
	return "logs"
}

// probeExisting asks the port whether a gateway is already answering.
//
// A stale responder (the previous process is releasing the socket) is reported
// as not-alive so start-up proceeds and retries the bind.
func probeExisting(cfg *server.Config) (bool, string) {
	host := cfg.Host
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d/health", host, cfg.Port)
	req, err := httpNewRequest(url)
	if err != nil {
		return false, ""
	}
	resp, err := httpDo(req, 2*time.Second)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	body, _ := readAll(resp)
	text := string(body)
	if strings.Contains(text, "qoder-proxy") {
		return true, text
	}
	return false, ""
}

func envTruthy(name string) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch raw {
	case "1", "on", "true", "yes", "enable", "enabled":
		return true
	}
	return false
}

// defaultPath resolves a relative path against the executable's directory.
func defaultPath(parts ...string) string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(parts...)
	}
	return filepath.Join(append([]string{filepath.Dir(exe)}, parts...)...)
}
