package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/cors"

	"github.com/pb-kecebong/backend/internal/config"
	"github.com/pb-kecebong/backend/internal/database"
	"github.com/pb-kecebong/backend/internal/httpx"
	"github.com/pb-kecebong/backend/internal/modules/auth"
	"github.com/pb-kecebong/backend/internal/modules/dashboard"
	financemod "github.com/pb-kecebong/backend/internal/modules/finance"
	"github.com/pb-kecebong/backend/internal/modules/inventory"
	"github.com/pb-kecebong/backend/internal/modules/mabar"
	"github.com/pb-kecebong/backend/internal/modules/period"
	"github.com/pb-kecebong/backend/internal/modules/player"
	"github.com/pb-kecebong/backend/internal/modules/report"
	"github.com/pb-kecebong/backend/internal/modules/simulator"
	"github.com/pb-kecebong/backend/internal/modules/venue"
)

func main() {
	config.LoadDotEnv(".env")
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
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

	playerSvc := player.NewService(pool)
	periodSvc := period.NewService(pool)
	mabarSvc := mabar.NewService(pool)
	invSvc := inventory.NewService(pool)
	finSvc := financemod.NewService(pool)
	venueSvc := venue.NewService(pool)
	dashSvc := dashboard.NewService(pool)
	reportSvc := report.NewService(pool)

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(cors.New(cors.Options{
		AllowedOrigins:   strings.Split(cfg.CORSOrigins, ","),
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowCredentials: true,
	}).Handler)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", authSvc.Login)

		r.Group(func(r chi.Router) {
			r.Use(httpx.RequireAuth(cfg.JWTSecret))

			r.Get("/auth/me", authSvc.Me)
			r.Post("/auth/logout", authSvc.Logout)

			r.Get("/dashboard", dashSvc.Get)

			r.Route("/players", func(r chi.Router) {
				r.Get("/", playerSvc.List)
				r.Post("/", playerSvc.Create)
				r.Get("/{id}", playerSvc.Get)
				r.Patch("/{id}", playerSvc.Update)
				r.Delete("/{id}", playerSvc.Delete)
			})

			r.Route("/periods", func(r chi.Router) {
				r.Get("/", periodSvc.List)
				r.Post("/", periodSvc.Create)
				r.Get("/{id}", periodSvc.Get)
				r.Patch("/{id}", periodSvc.Update)
				r.Delete("/{id}", periodSvc.Delete)
				r.Post("/{id}/complete", periodSvc.Complete)
				r.Post("/{id}/generate-sessions", periodSvc.GenerateSessions)
				r.Get("/{id}/members", periodSvc.ListMembers)
				r.Post("/{id}/members", periodSvc.AddMember)
				r.Delete("/{id}/members/{playerId}", periodSvc.RemoveMember)
				r.Get("/{id}/summary", periodSvc.Summary)
				r.Get("/{id}/session-breakdown", periodSvc.SessionBreakdown)
				r.Get("/{id}/shuttlecock-matrix", periodSvc.ShuttlecockMatrix)
			})

			r.Route("/mabar", func(r chi.Router) {
				r.Get("/", mabarSvc.List)
				r.Post("/", mabarSvc.Create)
				r.Get("/{id}", mabarSvc.Get)
				r.Patch("/{id}", mabarSvc.Update)
				r.Delete("/{id}", mabarSvc.Delete)
				r.Get("/{id}/attendance", mabarSvc.ListAttendance)
				r.Post("/{id}/attendance", mabarSvc.SetAttendance)
				r.Delete("/{id}/attendance/{playerId}", mabarSvc.RemoveAttendance)
				r.Get("/{id}/attendance/stats", mabarSvc.AttendanceStats)
				r.Get("/{id}/matches", mabarSvc.ListMatches)
				r.Post("/{id}/matches", mabarSvc.CreateMatch)
				r.Get("/{id}/simple-stats", mabarSvc.ListSimpleStats)
				r.Post("/{id}/simple-stats", mabarSvc.SaveSimpleStats)
				r.Post("/{id}/shuttlecock-allocation", mabarSvc.SaveAllocation)
				r.Get("/{id}/summary", mabarSvc.Summary)
				r.Get("/{id}/billing", mabarSvc.ListBilling)
				r.Post("/{id}/billing", mabarSvc.GenerateBilling)
				r.Patch("/{id}/billing/{playerId}", mabarSvc.UpdateBillingStatus)
			})

			r.Route("/matches", func(r chi.Router) {
				r.Patch("/{id}", mabarSvc.UpdateMatch)
				r.Delete("/{id}", mabarSvc.DeleteMatch)
				r.Post("/{id}/players", mabarSvc.AddMatchPlayer)
				r.Delete("/{id}/players/{playerId}", mabarSvc.RemoveMatchPlayer)
			})

			r.Route("/inventory/shuttlecock", func(r chi.Router) {
				r.Get("/", invSvc.ListProducts)
				r.Post("/", invSvc.CreateProduct)
				r.Patch("/{id}", invSvc.UpdateProduct)
				r.Delete("/{id}", invSvc.DeleteProduct)
				r.Get("/transactions", invSvc.Transactions)
				r.Post("/purchase", invSvc.Purchase)
				r.Post("/adjustment", invSvc.Adjust)
			})

			r.Route("/finance", func(r chi.Router) {
				r.Get("/summary", finSvc.Summary)
				r.Get("/transactions", finSvc.Transactions)
				r.Post("/revenue", finSvc.CreateRevenue)
				r.Post("/expense", finSvc.CreateExpense)
			})

			r.Route("/venues", func(r chi.Router) {
				r.Get("/", venueSvc.ListVenues)
				r.Post("/", venueSvc.CreateVenue)
			})
			r.Get("/courts", venueSvc.ListCourts)
			r.Post("/courts", venueSvc.CreateCourt)

			r.Route("/reports", func(r chi.Router) {
				r.Get("/financial", reportSvc.Financial)
				r.Get("/attendance", reportSvc.Attendance)
				r.Get("/no-show", reportSvc.NoShow)
				r.Get("/shuttlecock", reportSvc.Shuttlecock)
				r.Get("/player-usage", reportSvc.PlayerUsage)
				r.Get("/inactive-members", reportSvc.InactiveMembers)
			})

			r.Post("/simulator/period", simulator.RunPeriod)
			r.Post("/simulator/daily-event", simulator.RunDailyEvent)
		})
	})

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
