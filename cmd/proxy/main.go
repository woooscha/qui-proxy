package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/your-org/your-service/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.json", "configuration file")
	healthcheck := flag.Bool("healthcheck", false, "check local liveness")
	flag.Parse()
	f, err := os.Open(*configPath)
	if err != nil {
		slog.Error("opening configuration", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("closing configuration", "error", err)
		}
	}()
	var cfg proxy.Config
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		slog.Error("reading configuration", "error", err)
		os.Exit(1)
	}
	if *healthcheck {
		_, port, err := net.SplitHostPort(cfg.Listen)
		if err != nil || strings.TrimSpace(port) == "" {
			slog.Error("invalid listen address")
			os.Exit(1)
		}
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			slog.Error("health check failed", "error", err)
			os.Exit(1)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				slog.Warn("closing health response", "error", err)
			}
		}()
		if resp.StatusCode != http.StatusNoContent {
			os.Exit(1)
		}
		return
	}
	h, err := proxy.New(cfg)
	if err != nil {
		slog.Error("configuring proxy", "error", err)
		os.Exit(1)
	}
	s := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}
	slog.Info("starting proof-of-concept proxy", "listen", cfg.Listen)
	if err := s.ListenAndServe(); err != nil {
		slog.Error("serving", "error", err)
		os.Exit(1)
	}
}
