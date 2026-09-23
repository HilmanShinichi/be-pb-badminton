package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/cors"

	"github.com/pb-kecebong/backend/internal/config"
	"github.com/pb-kecebong/backend/internal/httpx"
	"github.com/pb-kecebong/backend/internal/modules/auth"
	"github.com/pb-kecebong/backend/internal/modules/dashboard"
	financemod "github.com/pb-kecebong/backend/internal/modules/finance"
	"github.com/pb-kecebong/backend/internal/modules/inventory"
	"github.com/pb-kecebong/backend/internal/modules/mabar"
	"github.com/pb-kecebong/backend/internal/modules/matchmaker"
	"github.com/pb-kecebong/backend/internal/modules/period"
	"github.com/pb-kecebong/backend/internal/modules/player"
	"github.com/pb-kecebong/backend/internal/modules/report"
	"github.com/pb-kecebong/backend/internal/modules/simulator"
	"github.com/pb-kecebong/backend/internal/modules/venue"
)

type Dependencies struct {
	Auth      *auth.Service
	Dashboard *dashboard.Service
	Finance   *financemod.Service
	Inventory *inventory.Service
	Mabar     *mabar.Service
	MatchMaker *matchmaker.Service
	Period    *period.Service
	Player    *player.Service
	Report    *report.Service
	Venue     *venue.Service
}

func NewRouter(deps Dependencies, cfg config.Config) http.Handler {
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
		r.Post("/auth/login", deps.Auth.Login)

		// Public live view: no token needed, read-only.
		r.Get("/public/match-events", deps.MatchMaker.PublicEvents)
		r.Get("/public/match-events/{id}", deps.MatchMaker.PublicEvent)
		r.Get("/public/schedule", deps.Mabar.List)

		r.Group(func(r chi.Router) {
			r.Use(httpx.RequireAuth(cfg.JWTSecret))
			r.Get("/auth/me", deps.Auth.Me)
			r.Post("/auth/logout", deps.Auth.Logout)
			r.Get("/dashboard", deps.Dashboard.Get)

			r.Route("/players", func(r chi.Router) {
				r.Get("/", deps.Player.List)
				r.Post("/", deps.Player.Create)
				r.Get("/{id}", deps.Player.Get)
				r.Patch("/{id}", deps.Player.Update)
				r.Delete("/{id}", deps.Player.Delete)
			})
			r.Route("/periods", func(r chi.Router) {
				r.Get("/", deps.Period.List)
				r.Post("/", deps.Period.Create)
				r.Get("/{id}", deps.Period.Get)
				r.Patch("/{id}", deps.Period.Update)
				r.Delete("/{id}", deps.Period.Delete)
				r.Post("/{id}/complete", deps.Period.Complete)
				r.Post("/{id}/generate-sessions", deps.Period.GenerateSessions)
				r.Get("/{id}/members", deps.Period.ListMembers)
				r.Post("/{id}/members", deps.Period.AddMember)
				r.Delete("/{id}/members/{playerId}", deps.Period.RemoveMember)
				r.Get("/{id}/summary", deps.Period.Summary)
				r.Get("/{id}/session-breakdown", deps.Period.SessionBreakdown)
				r.Get("/{id}/shuttlecock-matrix", deps.Period.ShuttlecockMatrix)
				r.Get("/{id}/attendance-matrix", deps.Period.AttendanceMatrix)
			})
			r.Route("/mabar", func(r chi.Router) {
				r.Get("/", deps.Mabar.List)
				r.Post("/", deps.Mabar.Create)
				r.Get("/{id}", deps.Mabar.Get)
				r.Patch("/{id}", deps.Mabar.Update)
				r.Delete("/{id}", deps.Mabar.Delete)
				r.Get("/{id}/attendance", deps.Mabar.ListAttendance)
				r.Post("/{id}/attendance", deps.Mabar.SetAttendance)
				r.Delete("/{id}/attendance/{playerId}", deps.Mabar.RemoveAttendance)
				r.Get("/{id}/attendance/stats", deps.Mabar.AttendanceStats)
				r.Get("/{id}/matches", deps.Mabar.ListMatches)
				r.Post("/{id}/matches", deps.Mabar.CreateMatch)
				r.Get("/{id}/simple-stats", deps.Mabar.ListSimpleStats)
				r.Post("/{id}/simple-stats", deps.Mabar.SaveSimpleStats)
				r.Post("/{id}/shuttlecock-allocation", deps.Mabar.SaveAllocation)
				r.Get("/{id}/summary", deps.Mabar.Summary)
				r.Get("/{id}/billing", deps.Mabar.ListBilling)
				r.Post("/{id}/billing", deps.Mabar.GenerateBilling)
				r.Patch("/{id}/billing/{playerId}", deps.Mabar.UpdateBillingStatus)
			})
			r.Route("/matches", func(r chi.Router) {
				r.Patch("/{id}", deps.Mabar.UpdateMatch)
				r.Delete("/{id}", deps.Mabar.DeleteMatch)
				r.Post("/{id}/players", deps.Mabar.AddMatchPlayer)
				r.Delete("/{id}/players/{playerId}", deps.Mabar.RemoveMatchPlayer)
			})
			r.Route("/match-events", func(r chi.Router) {
				r.Get("/", deps.MatchMaker.ListEvents)
				r.Post("/", deps.MatchMaker.CreateEvent)
				r.Get("/{id}", deps.MatchMaker.GetEvent)
				r.Patch("/{id}", deps.MatchMaker.UpdateEvent)
				r.Delete("/{id}", deps.MatchMaker.DeleteEvent)
				r.Post("/{id}/generate", deps.MatchMaker.Generate)
				r.Post("/{id}/players", deps.MatchMaker.AddEventPlayers)
			})
			r.Route("/generated-matches", func(r chi.Router) {
				r.Patch("/{id}", deps.MatchMaker.UpdateMatch)
			})
			r.Route("/inventory/shuttlecock", func(r chi.Router) {
				r.Get("/", deps.Inventory.ListProducts)
				r.Post("/", deps.Inventory.CreateProduct)
				r.Patch("/{id}", deps.Inventory.UpdateProduct)
				r.Delete("/{id}", deps.Inventory.DeleteProduct)
				r.Get("/transactions", deps.Inventory.Transactions)
				r.Post("/purchase", deps.Inventory.Purchase)
				r.Post("/adjustment", deps.Inventory.Adjust)
			})
			r.Route("/finance", func(r chi.Router) {
				r.Get("/summary", deps.Finance.Summary)
				r.Get("/transactions", deps.Finance.Transactions)
				r.Post("/revenue", deps.Finance.CreateRevenue)
				r.Post("/expense", deps.Finance.CreateExpense)
			})
			r.Route("/venues", func(r chi.Router) {
				r.Get("/", deps.Venue.ListVenues)
				r.Post("/", deps.Venue.CreateVenue)
			})
			r.Get("/courts", deps.Venue.ListCourts)
			r.Post("/courts", deps.Venue.CreateCourt)
			r.Route("/reports", func(r chi.Router) {
				r.Get("/financial", deps.Report.Financial)
				r.Get("/attendance", deps.Report.Attendance)
				r.Get("/no-show", deps.Report.NoShow)
				r.Get("/no-show-tracker", deps.Report.NoShowTracker)
				r.Delete("/no-show-tracker/{attendanceId}", deps.Report.DeleteNoShowIncident)
				r.Get("/shuttlecock", deps.Report.Shuttlecock)
				r.Get("/player-usage", deps.Report.PlayerUsage)
				r.Get("/inactive-members", deps.Report.InactiveMembers)
			})
			r.Post("/simulator/period", simulator.RunPeriod)
			r.Post("/simulator/daily-event", simulator.RunDailyEvent)
		})
	})

	return r
}
