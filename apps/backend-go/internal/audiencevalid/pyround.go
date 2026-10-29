package audiencevalid

import (
	"math"
	"math/big"
	"strconv"
)

// PyRound is Python's round(x, ndigits) for a float: correctly rounded on the
// float's exact decimal expansion, ties to even (round(0.05, 1) is 0.1 because
// 0.05 is slightly above one half, round(2.675, 2) is 2.67 because it is
// slightly below, round(2.5) is 2). The sign of a zero result is kept like
// Python (round(-0.04, 1) is -0.0).
func PyRound(x float64, ndigits int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	if ndigits >= 0 {
		if ndigits > 330 {
			return x
		}
		// FormatFloat rounds the exact binary value half-to-even.
		r, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', ndigits, 64), 64)
		return r
	}
	if ndigits < -330 {
		return math.Copysign(0, x)
	}
	// Round to a multiple of 10^-ndigits on the exact rational value.
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-ndigits)), nil)
	q := new(big.Rat).SetFloat64(x)
	q.Quo(q, new(big.Rat).SetInt(p))
	num, den := q.Num(), q.Denom()
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int)) // truncates toward zero
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	switch c := twice.Cmp(den); {
	case c > 0, c == 0 && quo.Bit(0) == 1:
		if num.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	f, _ := new(big.Float).SetInt(new(big.Int).Mul(quo, p)).Float64()
	if f == 0 {
		return math.Copysign(0, x)
	}
	return f
}
