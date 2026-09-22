package app

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/pb-kecebong/backend/internal/config"
	"github.com/pb-kecebong/backend/internal/database"
	"github.com/pb-kecebong/backend/internal/httpapi"
	"github.com/pb-kecebong/backend/internal/modules/auth"
	"github.com/pb-kecebong/backend/internal/modules/dashboard"
	financemod "github.com/pb-kecebong/backend/internal/modules/finance"
	"github.com/pb-kecebong/backend/internal/modules/inventory"
	"github.com/pb-kecebong/backend/internal/modules/mabar"
	"github.com/pb-kecebong/backend/internal/modules/matchmaker"
	"github.com/pb-kecebong/backend/internal/modules/period"
	"github.com/pb-kecebong/backend/internal/modules/player"
	"github.com/pb-kecebong/backend/internal/modules/report"
	"github.com/pb-kecebong/backend/internal/modules/venue"
)

// Run builds the application and owns its external resource lifecycle.
func Run(cfg config.Config) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseConnectionString())
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatalf("migration failed: %v", err)
	}

	authSvc := auth.NewService(pool, cfg.JWTSecret)
	if err := authSvc.EnsureAdminUser(ctx); err != nil {
		log.Fatalf("admin setup: %v", err)
	}

	r := httpapi.NewRouter(httpapi.Dependencies{
		Auth:      authSvc,
		Dashboard: dashboard.NewService(pool),
		Finance:   financemod.NewService(pool),
		Inventory: inventory.NewService(pool),
		Mabar:     mabar.NewService(pool),
		MatchMaker: matchmaker.NewService(pool, cfg),
		Period:    period.NewService(pool),
		Player:    player.NewService(pool),
		Report:    report.NewService(pool),
		Venue:     venue.NewService(pool),
	}, cfg)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("pb-kecebong backend listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
