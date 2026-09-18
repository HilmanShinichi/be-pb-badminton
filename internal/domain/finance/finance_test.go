package finance

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newRat(num, den int64) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(num), big.NewInt(den))
}

func TestRoundHalfUp(t *testing.T) {
	assert.Equal(t, int64(10417), RoundHalfUp(UnitShuttlecockCost(125000, 12)))
	assert.Equal(t, int64(1), RoundHalfUp(newRat(1, 2)))
	assert.Equal(t, int64(0), RoundHalfUp(newRat(-1, 3)))
}

func TestShuttlecockCost(t *testing.T) {
	// 29 units at 125000/12 per unit = 302083.33...
	assert.Equal(t, int64(302083), RoundHalfUp(ShuttlecockCost(125000, 12, 29)))
}

func TestCourtShares(t *testing.T) {
	assert.Equal(t, []int64{10000, 10000, 10000}, CourtShares(30000, 3))
	// 90000 / 7 = 12857.14... remainder 1 goes to the first player
	assert.Equal(t, []int64{12858, 12857, 12857, 12857, 12857, 12857, 12857}, CourtShares(90000, 7))
	assert.Equal(t, []int64{}, CourtShares(90000, 0))
	sum := int64(0)
	for _, s := range CourtShares(100001, 3) {
		sum += s
	}
	assert.Equal(t, int64(100001), sum)
}

func TestPeriodMemberBill(t *testing.T) {
	assert.Equal(t, int64(95000), PeriodMemberBill(45000, 10000, 5))
	assert.Equal(t, int64(45000), PeriodMemberBill(45000, 10000, 0))
}

func TestNonMemberAndBenefit(t *testing.T) {
	nonMember := NonMemberBill(25000, 5)
	assert.Equal(t, int64(125000), nonMember)
	assert.Equal(t, int64(30000), nonMember-PeriodMemberBill(45000, 10000, 5))
}

func TestBreakEvenAttendance(t *testing.T) {
	assert.Equal(t, int64(3), BreakEvenAttendance(45000, 10000, 25000))
	assert.Equal(t, int64(0), BreakEvenAttendance(0, 10000, 25000))
	assert.Equal(t, int64(-1), BreakEvenAttendance(45000, 25000, 25000))
}

func TestFinancialStatus(t *testing.T) {
	assert.Equal(t, "PROFIT", FinancialStatus(430000, 395083))
	assert.Equal(t, "LOSS", FinancialStatus(100000, 200000))
	assert.Equal(t, "BREAK_EVEN", FinancialStatus(100000, 100000))
}

func TestDailyPlayerBill(t *testing.T) {
	assert.Equal(t, int64(19000), DailyPlayerBill(10000, 3, 3000, 0))
}

func TestNoShowRate(t *testing.T) {
	assert.Equal(t, int64(300), NoShowRate(3, 10)) // 30.0%
	assert.Equal(t, int64(0), NoShowRate(0, 0))
	assert.Equal(t, int64(333), NoShowRate(1, 3)) // 33.3%
}
