package kubelet

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// maxExponent bounds the exponent a quantity may carry.
//
// The API server accepts and stores "1e1000000", and expanding that costs a
// million digits of arithmetic before the result can be rejected for not
// fitting in a byte count. Every watch pass would pay it again for as long as
// the pod exists. A byte count needs about 19 digits, so anything past this is
// refused on sight.
const maxExponent = 64

// maxQuantityLen bounds how long a quantity may be.
//
// The exponent is not the only way to ask for a lot of arithmetic. The API
// server stores a hundred thousand digits verbatim rather than clamping them,
// and parsing that costs 10ms where a million digits costs 780ms, once per pod
// per pass for as long as the pod lives. The largest count of bytes is 19
// digits, so anything approaching this is not a request.
const maxQuantityLen = 64

// suffixes are the Kubernetes quantity suffixes, longest first so that Ei is
// matched before E. A suffix only ever ends the string, which is what keeps
// the exa suffix in "1E" apart from the exponent in "1E3".
var suffixes = []struct {
	suffix string
	// num and exp give the multiplier as num * 10^exp, which covers the
	// binary powers and the sub-unit suffixes without floating point.
	num int64
	exp int
}{
	{"Ki", 1 << 10, 0}, {"Mi", 1 << 20, 0}, {"Gi", 1 << 30, 0},
	{"Ti", 1 << 40, 0}, {"Pi", 1 << 50, 0}, {"Ei", 1 << 60, 0},
	{"n", 1, -9}, {"u", 1, -6}, {"m", 1, -3},
	{"k", 1, 3}, {"M", 1, 6}, {"G", 1, 9},
	{"T", 1, 12}, {"P", 1, 15}, {"E", 1, 18},
}

// ParseQuantity reads a Kubernetes quantity as a whole number of bytes.
//
// It covers what the API server actually stores, which is wider than the
// integer-with-a-suffix form a request is usually written in: the server keeps
// "1500m" and "1e3" verbatim, and canonicalises "1023.9Mi" to
// "1073636966400m". A parser that read only the usual form would refuse those,
// and refusing is not free, since the caller loses the pod.
//
// Fractions round up. A request is a claim on space, and rounding a claim down
// is the direction that under-reclaims.
func ParseQuantity(s string) (int64, error) {
	// The original is kept for errors. Everything below works on the number
	// with its suffix removed, and an operator reading the message is looking
	// at the field as it was written.
	whole := strings.TrimSpace(s)
	if whole == "" {
		return 0, fmt.Errorf("quantity is empty")
	}
	// Checked on the raw text, before anything looks at what it says. The
	// point is to refuse the arithmetic, so it has to happen first.
	if len(whole) > maxQuantityLen {
		return 0, fmt.Errorf("quantity is %d characters long, which no count of bytes needs", len(whole))
	}
	num, exp := int64(1), 0
	number := whole
	for _, u := range suffixes {
		if rest, ok := strings.CutSuffix(number, u.suffix); ok {
			number, num, exp = rest, u.num, u.exp
			break
		}
	}
	if number == "" {
		return 0, fmt.Errorf("quantity %q is a suffix with no number before it", whole)
	}
	mantissa, dexp, err := split(number, whole)
	if err != nil {
		return 0, err
	}
	r, ok := new(big.Rat).SetString(mantissa)
	if !ok {
		return 0, fmt.Errorf("quantity %q is not a number", whole)
	}
	if r.Sign() < 0 {
		return 0, fmt.Errorf("quantity %q is negative", whole)
	}
	exp += dexp
	r.Mul(r, new(big.Rat).SetInt64(num))
	r.Mul(r, new(big.Rat).SetFrac(pow10(exp), pow10(-exp)))
	return ceilBytes(r, whole)
}

// split separates a number from its exponent and refuses anything a quantity
// never carries.
//
// The exponent is taken out here rather than left to the number parser so that
// its size can be checked before any arithmetic depends on it.
func split(number, whole string) (mantissa string, exp int, err error) {
	mantissa = number
	if i := strings.IndexAny(number, "eE"); i >= 0 {
		mantissa = number[:i]
		exp, err = strconv.Atoi(number[i+1:])
		if err != nil {
			return "", 0, fmt.Errorf("quantity %q has an exponent that is not a whole number", whole)
		}
		if exp > maxExponent || exp < -maxExponent {
			return "", 0, fmt.Errorf("quantity %q has an exponent no count of bytes could use", whole)
		}
	}
	// Checked explicitly, because the number parser underneath also accepts
	// forms a quantity never takes, such as "1/3" and "0x10", and reading a
	// number out of one would accept what Kubernetes would have rejected.
	for _, c := range mantissa {
		if (c < '0' || c > '9') && c != '.' && c != '+' && c != '-' {
			return "", 0, fmt.Errorf("quantity %q is not a number", whole)
		}
	}
	if mantissa == "" {
		return "", 0, fmt.Errorf("quantity %q is not a number", whole)
	}
	return mantissa, exp, nil
}

// pow10 gives 10^n for a non-negative n and 1 otherwise, so a negative
// exponent can be expressed as the denominator of a fraction.
func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// ceilBytes rounds a quantity up to whole bytes, refusing one too large to
// count rather than wrapping into a small number that would look like no
// demand at all.
func ceilBytes(r *big.Rat, whole string) (int64, error) {
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("quantity %q does not fit in a signed 64-bit count of bytes", whole)
	}
	return q.Int64(), nil
}

// addSaturating adds two byte counts without wrapping.
//
// A request the API server clamped to the largest signed 64-bit value is a
// real thing to receive, and adding anything to it wraps negative, which reads
// as no demand at all.
func addSaturating(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}
