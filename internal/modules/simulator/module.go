package simulator

import (
	"math/big"
	"net/http"

	"github.com/pb-kecebong/backend/internal/domain/finance"
	"github.com/gofiber/fiber/v2"

	"github.com/pb-kecebong/backend/internal/httpx"
)

// RunPeriod answers "is this period profitable?" from typed inputs, before any
// data is committed. PRD §42.
func RunPeriod(c *fiber.Ctx) error {
	var in struct {
		Sessions            int   `json:"sessions"`
		Members             int   `json:"members"`
		AvgMemberAttendance int   `json:"avg_member_attendance"`
		NonMembers          int   `json:"non_members"`
		CommitmentFee       int64 `json:"commitment_fee"`
		MemberContribution  int64 `json:"member_contribution"`
		NonMemberFee        int64 `json:"non_member_fee"`
		VenueCostPerSession int64 `json:"venue_cost_per_session"`
		ShuttlePerSession   int64 `json:"shuttlecock_per_session"`
		PackPrice           int64 `json:"pack_price"`
		UnitsPerPack        int64 `json:"units_per_pack"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if err := validatePositive(map[string]int64{
		"Sessions": int64(in.Sessions), "Members": int64(in.Members), "Non-member rate": in.NonMemberFee,
		"Court cost per session": in.VenueCostPerSession, "Shuttlecock pack": in.PackPrice, "Units per pack": in.UnitsPerPack,
	}); err != "" {
		return httpx.WriteAppError(c, httpx.Unprocessable(err))
	}
	attendance := in.Members
	if in.AvgMemberAttendance > 0 {
		attendance = in.AvgMemberAttendance
	}

	memberRevenue := int64(in.Members)*in.CommitmentFee + int64(attendance)*in.MemberContribution*int64(in.Sessions)
	nonMemberRevenue := int64(in.NonMembers) * in.NonMemberFee * int64(in.Sessions)
	revenue := memberRevenue + nonMemberRevenue

	venueTotal := in.VenueCostPerSession * int64(in.Sessions)
	units := in.ShuttlePerSession * int64(in.Sessions)
	shuttleCost := finance.RoundHalfUp(finance.ShuttlecockCost(in.PackPrice, in.UnitsPerPack, units))
	operatingCost := venueTotal + shuttleCost
	profit := revenue - operatingCost

	packs := int64(0)
	if in.UnitsPerPack > 0 {
		packs = ceilDiv(units, in.UnitsPerPack)
	}
	purchaseCash := packs * in.PackPrice

	// Required non-members per session for the period to break even.
	requiredNonMembers := int64(0)
	if in.NonMemberFee > 0 && nonMemberRevenue < operatingCost {
		gap := operatingCost - memberRevenue
		if gap > 0 {
			requiredNonMembers = ceilDiv(gap, in.NonMemberFee*int64(in.Sessions))
		}
	}

	memberTotal := finance.PeriodMemberBill(in.CommitmentFee, in.MemberContribution, int64(in.Sessions))
	nonMemberTotal := finance.NonMemberBill(in.NonMemberFee, int64(in.Sessions))

	// Attendance table from PRD §29, member vs non-member side by side.
	attendanceTable := []map[string]any{}
	for n := int64(1); n <= int64(in.Sessions); n++ {
		m := finance.PeriodMemberBill(in.CommitmentFee, in.MemberContribution, n)
		nm := finance.NonMemberBill(in.NonMemberFee, n)
		attendanceTable = append(attendanceTable, map[string]any{
			"attendance": n, "member_total": m, "non_member_total": nm, "member_benefit": nm - m,
		})
	}

	return httpx.OK(c, http.StatusOK, map[string]any{
		"revenue":               revenue,
		"member_revenue":        memberRevenue,
		"non_member_revenue":    nonMemberRevenue,
		"venue_total":           venueTotal,
		"shuttlecock_units":     units,
		"shuttlecock_cost":      shuttleCost,
		"operating_cost":        operatingCost,
		"operating_profit":      profit,
		"status":                finance.FinancialStatus(revenue, operatingCost),
		"packs_needed":          packs,
		"purchase_cash":         purchaseCash,
		"cash_flow":             revenue - venueTotal - purchaseCash,
		"break_even_attendance": finance.BreakEvenAttendance(in.CommitmentFee, in.MemberContribution, in.NonMemberFee),
		"member_total_cost":     memberTotal,
		"non_member_total_cost": nonMemberTotal,
		"required_non_members":  requiredNonMembers,
		"attendance_table":      attendanceTable,
	})
}

// RunDailyEvent projects one daily/event mabar: equal court split plus
// per-shuttlecock contribution. PRD §25/§26.
func RunDailyEvent(c *fiber.Ctx) error {
	var in struct {
		VenueCost           int64 `json:"venue_cost"`
		Players             int   `json:"players"`
		PricePerShuttlecock int64 `json:"price_per_shuttlecock"`
		ShuttlePerPlayer    int64 `json:"shuttlecock_per_player"`
		PackPrice           int64 `json:"pack_price"`
		UnitsPerPack        int64 `json:"units_per_pack"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if in.Players <= 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Player count must be greater than 0."))
	}
	if in.VenueCost < 0 || in.PricePerShuttlecock < 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Cost must not be negative."))
	}

	shares := finance.CourtShares(in.VenueCost, in.Players)
	totalUnits := in.ShuttlePerPlayer * int64(in.Players)
	shuttleCost := finance.RoundHalfUp(finance.ShuttlecockCost(in.PackPrice, in.UnitsPerPack, totalUnits))

	shuttleRevenue := totalUnits * in.PricePerShuttlecock
	revenue := in.VenueCost + shuttleRevenue
	operatingCost := in.VenueCost + shuttleCost

	return httpx.OK(c, http.StatusOK, map[string]any{
		"court_share":         shares[0],
		"court_cost_total":    in.VenueCost,
		"shuttlecock_units":   totalUnits,
		"shuttlecock_cost":    shuttleCost,
		"shuttlecock_revenue": shuttleRevenue,
		"revenue":             revenue,
		"operating_cost":      operatingCost,
		"operating_profit":    revenue - operatingCost,
		"status":              finance.FinancialStatus(revenue, operatingCost),
		"example_bill": map[string]any{
			"court_share":              shares[0],
			"shuttlecock_count":        in.ShuttlePerPlayer,
			"shuttlecock_contribution": in.ShuttlePerPlayer * in.PricePerShuttlecock,
			"total":                    finance.DailyPlayerBill(shares[0], in.ShuttlePerPlayer, in.PricePerShuttlecock, 0),
		},
	})
}

func validatePositive(fields map[string]int64) string {
	for label, v := range fields {
		if v <= 0 {
			return label + " must be greater than 0."
		}
	}
	return ""
}

func ceilDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	q := new(big.Int).Div(big.NewInt(a), big.NewInt(b))
	r := a % b
	if r != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}
