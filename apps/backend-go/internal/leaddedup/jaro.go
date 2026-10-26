package leaddedup

import "math"

// JaroWinkler ports _jaro_similarity/jaro_winkler over code points. The
// explicit float64 conversions stop the compiler fusing multiply-adds so the
// result is bit-identical to CPython's double arithmetic.
func JaroWinkler(a, b string) float64 { return jaroWinklerRunes([]rune(a), []rune(b)) }

func jaroRunes(s1, s2 []rune) float64 {
	if string(s1) == string(s2) {
		return 1.0
	}
	len1, len2 := len(s1), len(s2)
	if len1 == 0 || len2 == 0 {
		return 0.0
	}
	md := max(len1, len2)/2 - 1
	if md < 0 {
		md = 0
	}
	m1 := make([]bool, len1)
	m2 := make([]bool, len2)
	matches, trans := 0, 0
	for i := 0; i < len1; i++ {
		start := max(0, i-md)
		end := min(i+md+1, len2)
		for j := start; j < end; j++ {
			if m2[j] || s1[i] != s2[j] {
				continue
			}
			m1[i], m2[j] = true, true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0.0
	}
	k := 0
	for i := 0; i < len1; i++ {
		if !m1[i] {
			continue
		}
		for !m2[k] {
			k++
		}
		if s1[i] != s2[k] {
			trans++
		}
		k++
	}
	m := float64(matches)
	a := float64(m / float64(len1))
	b := float64(m / float64(len2))
	c := float64(m-float64(trans)/2) / m
	return float64(float64(a+b)+c) / 3
}

func jaroWinklerRunes(s1, s2 []rune) float64 {
	j := jaroRunes(s1, s2)
	pl := 0
	for i := 0; i < min(len(s1), len(s2), 4); i++ {
		if s1[i] != s2[i] {
			break
		}
		pl++
	}
	return j + float64(float64(float64(pl)*0.1)*(1-j))
}

// round4 etc. mirror Python's round(x, n): exact decimal rounding.
func pyRound(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	return roundDecimal(x, n)
}
