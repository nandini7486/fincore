// Package money converts between FinCore's internal representation of money
// (int64 minor units: paise for INR) and the decimal strings used in the API.
//
// Money is never a float anywhere in FinCore: floats cannot represent most
// decimal fractions exactly (0.1 + 0.2 != 0.3), and rounding errors in a
// ledger are bugs.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MinorUnitsPerMajor is the number of paise in one rupee.
const MinorUnitsPerMajor = 100

// MaxAmount is the largest single payment or top-up: ₹1,00,00,000.00
// (one crore) in paise. It keeps amounts far from int64 overflow even when
// many are summed, and rejects obviously mistaken inputs.
const MaxAmount int64 = 1_00_00_000 * MinorUnitsPerMajor

// ErrInvalidAmount is returned by Parse for any amount that is not allowed.
var ErrInvalidAmount = errors.New("invalid amount")

// Parse converts an API amount such as "2000.00" or "0.5" into minor units.
//
// Accepted: digits, optionally followed by '.' and one or two digits.
// Rejected: signs, spaces, exponents ("1e3"), commas, more than two decimal
// places ("1.005" would need rounding, and silently rounding money is a bug),
// leading zeros ("007"), zero, and anything above MaxAmount.
func Parse(s string) (int64, error) {
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) {
		return 0, fmt.Errorf("%w: %q is not a decimal amount", ErrInvalidAmount, s)
	}
	if len(whole) > 1 && whole[0] == '0' {
		return 0, fmt.Errorf("%w: %q has leading zeros", ErrInvalidAmount, s)
	}
	if hasDot && (len(frac) < 1 || len(frac) > 2 || !allDigits(frac)) {
		return 0, fmt.Errorf("%w: %q must have one or two decimal places", ErrInvalidAmount, s)
	}
	// Longer than MaxAmount's whole part can never be valid; checking the
	// length first also keeps ParseInt far from overflow.
	if len(whole) > len(strconv.FormatInt(MaxAmount/MinorUnitsPerMajor, 10)) {
		return 0, fmt.Errorf("%w: %q exceeds the maximum", ErrInvalidAmount, s)
	}

	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	var f int64
	if frac != "" {
		f, _ = strconv.ParseInt(frac, 10, 64) // digits only, at most 2: cannot fail
		if len(frac) == 1 {
			f *= 10 // "0.5" means 50 paise
		}
	}

	minor := w*MinorUnitsPerMajor + f
	if minor <= 0 {
		return 0, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidAmount)
	}
	if minor > MaxAmount {
		return 0, fmt.Errorf("%w: %q exceeds the maximum of %s", ErrInvalidAmount, s, Format(MaxAmount))
	}
	return minor, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Format renders minor units as a decimal string with exactly two fraction
// digits, e.g. 123456 -> "1234.56", -50 -> "-0.50".
func Format(minor int64) string {
	neg := minor < 0
	// Convert through uint64 so math.MinInt64 does not overflow on negation.
	u := uint64(minor)
	if neg {
		u = -u
	}
	whole := u / MinorUnitsPerMajor
	frac := u % MinorUnitsPerMajor

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatUint(whole, 10))
	b.WriteByte('.')
	if frac < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatUint(frac, 10))
	return b.String()
}
