package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/config"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/events"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/httpapi"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/labels"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/outbox"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/store"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/tenants"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/wa"
)

const (
	defaultPort = "3006"
	serviceName = "squamata-whatsapp"
)

func main() {
	healthcheck := flag.Bool("health", false, "consulta /api/v1/health e encerra com 0 (saudável) ou 1")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("serviço encerrado com erro", "erro", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repo := sessions.NewRepo(db.SQL)
	if err := repo.Migrate(ctx); err != nil {
		return err
	}

	outboxRepo := outbox.NewRepo(db.SQL)
	if err := outboxRepo.EnsureSchema(ctx); err != nil {
		return err
	}

	labelRepo := labels.NewRepo(db.SQL)
	if err := labelRepo.EnsureSchema(ctx); err != nil {
		return err
	}

	tenantRepo := tenants.NewRepo(db.SQL)
	if err := tenantRepo.EnsureSchema(ctx); err != nil {
		return err
	}

	dispatcher := events.NewDispatcher(cfg.WebhookURL, cfg.WebhookSecret, outboxRepo, logger)
	// Multi-SaaS: cada tenant publica no próprio webhook; o global é fallback.
	dispatcher.WithResolver(tenantRepo)
	if !dispatcher.Enabled() {
		logger.Warn("webhook desligado: nenhum tenant com webhook e SQUAMATA_WEBHOOK_URL vazio")
	}
	go dispatcher.Run(ctx)

	manager := wa.New(db.Container, repo, labelRepo, dispatcher, logger)
	defer manager.Shutdown()

	api := httpapi.New(repo, db, manager, tenantRepo, cfg.InternalToken, logger)
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("serviço iniciado", "servico", serviceName, "porta", cfg.Port, "banco", cfg.DBPath)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	// Reconexão das sessões já pareadas, em paralelo ao servidor HTTP.
	go manager.Start(ctx)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("desligar servidor HTTP: %w", err)
	}

	logger.Info("serviço encerrado", "servico", serviceName)
	return nil
}

// runHealthcheck existe porque a imagem final é distroless: não há curl nem
// shell para o HEALTHCHECK do compose. O próprio binário faz o GET.
func runHealthcheck() int {
	port := os.Getenv("SQUAMATA_WHATSAPP_PORT")
	if port == "" {
		port = defaultPort
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/api/v1/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck falhou:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck retornou status", resp.StatusCode)
		return 1
	}
	return 0
}
