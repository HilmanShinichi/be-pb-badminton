// Package finance holds all money math for the club. Every figure here is
// backend-authoritative: the frontend only renders results.
//
// Money is carried as int64 rupiah. Where a division is not exact
// (per-unit shuttlecock cost, cost sharing) calculation stays in big.Rat and
// is rounded exactly once, at the boundary this package defines.
package finance

import (
	"math/big"
)

// RoundHalfUp rounds a rational to the nearest integer, halves away from zero.
func RoundHalfUp(r *big.Rat) int64 {
	q := new(big.Int)
	m := new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), m)
	m2 := new(big.Int).Abs(m)
	m2.Lsh(m2, 1)
	if m2.Cmp(new(big.Int).Abs(r.Denom())) >= 0 {
		if r.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return q.Int64()
}

// UnitShuttlecockCost returns the per-unit cost of a pack as an exact rational.
// Example: pack 125000 with 12 units -> 125000/12, kept unrounded.
func UnitShuttlecockCost(packPrice, unitsPerPack int64) *big.Rat {
	if unitsPerPack <= 0 {
		return new(big.Rat)
	}
	return new(big.Rat).SetFrac(big.NewInt(packPrice), big.NewInt(unitsPerPack))
}

// ShuttlecockCost is the exact cost of n used units; round only for display or
// finalized accounting entries.
func ShuttlecockCost(packPrice, unitsPerPack, units int64) *big.Rat {
	return new(big.Rat).Mul(UnitShuttlecockCost(packPrice, unitsPerPack), new(big.Rat).SetInt64(units))
}

// CourtShares splits total court cost equally among present players.
// Any rounding remainder of the total is distributed one rupiah at a time to
// the earliest players in the slice, so the shares always sum exactly to total.
func CourtShares(total int64, players int) []int64 {
	shares := make([]int64, players)
	if players <= 0 {
		return shares
	}
	base := total / int64(players)
	for i := range shares {
		shares[i] = base
	}
	remainder := total - base*int64(players)
	for i := 0; remainder > 0; i, remainder = i+1, remainder-1 {
		shares[i]++
	}
	return shares
}

// PeriodMemberBill: commitment fee once per period plus contribution per
// session actually attended. PRD §28.
func PeriodMemberBill(commitmentFee, contribution int64, attendance int64) int64 {
	return commitmentFee + contribution*attendance
}

// NonMemberBill: flat fee per attendance, no commitment. PRD §28.
func NonMemberBill(fee int64, attendance int64) int64 {
	return fee * attendance
}

// BreakEvenAttendance is the smallest attendance count where the member
// formula costs no more than the non-member formula. Returns -1 when the
// member path never breaks even (contribution >= non-member fee and a
// positive commitment fee).
func BreakEvenAttendance(commitmentFee, contribution, nonMemberFee int64) int64 {
	if nonMemberFee <= contribution {
		if commitmentFee <= 0 {
			return 0
		}
		return -1
	}
	n := commitmentFee / (nonMemberFee - contribution)
	if commitmentFee%(nonMemberFee-contribution) != 0 {
		n++
	}
	return n
}

// FinancialStatus classifies revenue against operating cost. PRD §35.
func FinancialStatus(revenue, operatingCost int64) string {
	switch {
	case revenue > operatingCost:
		return "PROFIT"
	case revenue < operatingCost:
		return "LOSS"
	default:
		return "BREAK_EVEN"
	}
}

// DailyPlayerBill is one player's bill for a daily/event mabar: equal court
// share plus a fixed contribution per shuttlecock they played. PRD §27.
func DailyPlayerBill(courtShare, shuttlecockCount, pricePerShuttlecock, otherCharge int64) int64 {
	return courtShare + shuttlecockCount*pricePerShuttlecock + otherCharge
}

// NoShowRate: no-shows over total lists, in tenths of a percent
// (30.0% -> 300) so the value stays an integer.
func NoShowRate(noShow, totalListed int64) int64 {
	if totalListed == 0 {
		return 0
	}
	return RoundHalfUp(new(big.Rat).SetFrac(big.NewInt(noShow*1000), big.NewInt(totalListed)))
}
