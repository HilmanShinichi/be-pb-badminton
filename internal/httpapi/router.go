package httpapi

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"

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
	Auth       *auth.Service
	Dashboard  *dashboard.Service
	Finance    *financemod.Service
	Inventory  *inventory.Service
	Mabar      *mabar.Service
	MatchMaker *matchmaker.Service
	Period     *period.Service
	Player     *player.Service
	Report     *report.Service
	Venue      *venue.Service
}

func NewRouter(deps Dependencies, cfg config.Config) *fiber.App {
	app := fiber.New(fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	})
	app.Use(requestid.New())
	app.Use(recover.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins:     strings.Join(strings.Split(cfg.CORSOrigins, ","), ","),
		AllowMethods:     "GET,POST,PATCH,DELETE,OPTIONS",
		AllowHeaders:     "Authorization,Content-Type",
		AllowCredentials: true,
	}))

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	v1 := app.Group("/api/v1")
	v1.Post("/auth/login", deps.Auth.Login)

	// Public live view: no token needed, read-only.
	v1.Get("/public/match-events", deps.MatchMaker.PublicEvents)
	v1.Get("/public/match-events/:id", deps.MatchMaker.PublicEvent)
	v1.Get("/public/schedule", deps.Mabar.List)

	authed := v1.Group("", httpx.RequireAuth(cfg.JWTSecret))
	authed.Get("/auth/me", deps.Auth.Me)
	authed.Post("/auth/logout", deps.Auth.Logout)
	authed.Get("/dashboard", deps.Dashboard.Get)

	players := authed.Group("/players")
	players.Get("/", deps.Player.List)
	players.Post("/", deps.Player.Create)
	players.Get("/:id", deps.Player.Get)
	players.Patch("/:id", deps.Player.Update)
	players.Delete("/:id", deps.Player.Delete)

	periods := authed.Group("/periods")
	periods.Get("/", deps.Period.List)
	periods.Post("/", deps.Period.Create)
	periods.Get("/:id", deps.Period.Get)
	periods.Patch("/:id", deps.Period.Update)
	periods.Delete("/:id", deps.Period.Delete)
	periods.Post("/:id/complete", deps.Period.Complete)
	periods.Post("/:id/generate-sessions", deps.Period.GenerateSessions)
	periods.Get("/:id/members", deps.Period.ListMembers)
	periods.Post("/:id/members", deps.Period.AddMember)
	periods.Delete("/:id/members/:playerId", deps.Period.RemoveMember)
	periods.Get("/:id/summary", deps.Period.Summary)
	periods.Get("/:id/session-breakdown", deps.Period.SessionBreakdown)
	periods.Get("/:id/shuttlecock-matrix", deps.Period.ShuttlecockMatrix)
	periods.Get("/:id/attendance-matrix", deps.Period.AttendanceMatrix)

	mabar := authed.Group("/mabar")
	mabar.Get("/", deps.Mabar.List)
	mabar.Post("/", deps.Mabar.Create)
	mabar.Get("/:id", deps.Mabar.Get)
	mabar.Patch("/:id", deps.Mabar.Update)
	mabar.Delete("/:id", deps.Mabar.Delete)
	mabar.Get("/:id/attendance", deps.Mabar.ListAttendance)
	mabar.Post("/:id/attendance", deps.Mabar.SetAttendance)
	mabar.Delete("/:id/attendance/:playerId", deps.Mabar.RemoveAttendance)
	mabar.Get("/:id/attendance/stats", deps.Mabar.AttendanceStats)
	mabar.Get("/:id/matches", deps.Mabar.ListMatches)
	mabar.Post("/:id/matches", deps.Mabar.CreateMatch)
	mabar.Get("/:id/simple-stats", deps.Mabar.ListSimpleStats)
	mabar.Post("/:id/simple-stats", deps.Mabar.SaveSimpleStats)
	mabar.Post("/:id/shuttlecock-allocation", deps.Mabar.SaveAllocation)
	mabar.Get("/:id/summary", deps.Mabar.Summary)
	mabar.Get("/:id/billing", deps.Mabar.ListBilling)
	mabar.Post("/:id/billing", deps.Mabar.GenerateBilling)
	mabar.Patch("/:id/billing/:playerId", deps.Mabar.UpdateBillingStatus)

	matches := authed.Group("/matches")
	matches.Patch("/:id", deps.Mabar.UpdateMatch)
	matches.Delete("/:id", deps.Mabar.DeleteMatch)
	matches.Post("/:id/players", deps.Mabar.AddMatchPlayer)
	matches.Delete("/:id/players/:playerId", deps.Mabar.RemoveMatchPlayer)

	events := authed.Group("/match-events")
	events.Get("/", deps.MatchMaker.ListEvents)
	events.Post("/", deps.MatchMaker.CreateEvent)
	events.Get("/:id", deps.MatchMaker.GetEvent)
	events.Patch("/:id", deps.MatchMaker.UpdateEvent)
	events.Delete("/:id", deps.MatchMaker.DeleteEvent)
		events.Post("/:id/generate", deps.MatchMaker.Generate)
		events.Post("/:id/players", deps.MatchMaker.AddEventPlayers)
		events.Delete("/:id/rounds/:round", deps.MatchMaker.DeleteRound)

	genMatches := authed.Group("/generated-matches")
	genMatches.Patch("/:id", deps.MatchMaker.UpdateMatch)

	inv := authed.Group("/inventory/shuttlecock")
	inv.Get("/", deps.Inventory.ListProducts)
	inv.Post("/", deps.Inventory.CreateProduct)
	inv.Patch("/:id", deps.Inventory.UpdateProduct)
	inv.Delete("/:id", deps.Inventory.DeleteProduct)
	inv.Get("/transactions", deps.Inventory.Transactions)
	inv.Post("/purchase", deps.Inventory.Purchase)
	inv.Post("/adjustment", deps.Inventory.Adjust)

	finance := authed.Group("/finance")
	finance.Get("/summary", deps.Finance.Summary)
	finance.Get("/transactions", deps.Finance.Transactions)
	finance.Post("/revenue", deps.Finance.CreateRevenue)
	finance.Post("/expense", deps.Finance.CreateExpense)

	venues := authed.Group("/venues")
	venues.Get("/", deps.Venue.ListVenues)
	venues.Post("/", deps.Venue.CreateVenue)
	authed.Get("/courts", deps.Venue.ListCourts)
	authed.Post("/courts", deps.Venue.CreateCourt)

	reports := authed.Group("/reports")
	reports.Get("/financial", deps.Report.Financial)
	reports.Get("/attendance", deps.Report.Attendance)
	reports.Get("/no-show", deps.Report.NoShow)
	reports.Get("/no-show-tracker", deps.Report.NoShowTracker)
	reports.Delete("/no-show-tracker/:attendanceId", deps.Report.DeleteNoShowIncident)
	reports.Get("/shuttlecock", deps.Report.Shuttlecock)
	reports.Get("/player-usage", deps.Report.PlayerUsage)
	reports.Get("/inactive-members", deps.Report.InactiveMembers)

	authed.Post("/simulator/period", simulator.RunPeriod)
	authed.Post("/simulator/daily-event", simulator.RunDailyEvent)

	return app
}
