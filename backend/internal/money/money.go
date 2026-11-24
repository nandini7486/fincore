// Package money converts between FinCore's internal representation of money
// (int64 minor units: paise for INR) and the decimal strings used in the API.
//
// Money is never a float anywhere in FinCore: floats cannot represent most
// decimal fractions exactly (0.1 + 0.2 != 0.3), and rounding errors in a
// ledger are bugs. Parsing request amounts arrives with payments (Phase 2c).
package money

import (
	"strconv"
	"strings"
)

// MinorUnitsPerMajor is the number of paise in one rupee.
const MinorUnitsPerMajor = 100

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
