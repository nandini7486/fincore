package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/money"
)

func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{10, "0.10"},
		{99, "0.99"},
		{100, "1.00"},
		{123456, "1234.56"},
		{200000, "2000.00"},
		{-50, "-0.50"},
		{-500000, "-5000.00"}, // the treasury goes negative as it issues money
		{math.MaxInt64, "92233720368547758.07"},
		{math.MinInt64, "-92233720368547758.08"},
	} {
		if got := money.Format(tc.minor); got != tc.want {
			t.Errorf("Format(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"1", 100},
		{"0.01", 1},
		{"0.5", 50},
		{"0.50", 50},
		{"2000.00", 200000},
		{"1234.56", 123456},
		{"10000000", money.MaxAmount},
		{"10000000.00", money.MaxAmount},
	} {
		got, err := money.Parse(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"", " ", "0", "0.0", "0.00", // zero
		"-1", "+1", "-0.01", // signs
		"1.", ".5", "1.005", "1.234", // decimal places
		"1e3", "1E3", "0x10", "1,000", "1_000", "١", // other notations
		" 1", "1 ", "1.0 ", // whitespace
		"007", "00.50", // leading zeros
		"10000000.01", "99999999", "9223372036854775807", "99999999999999999999999", // above maximum
		"NaN", "Inf", "abc", "1.a",
	} {
		if got, err := money.Parse(in); !errors.Is(err, money.ErrInvalidAmount) {
			t.Errorf("Parse(%q) = %d, %v; want ErrInvalidAmount", in, got, err)
		}
	}
}

// Every valid amount survives a Format -> Parse round trip unchanged.
func FuzzParseFormatRoundTrip(f *testing.F) {
	for _, seed := range []int64{1, 50, 99, 100, 123456, money.MaxAmount} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, minor int64) {
		if minor <= 0 || minor > money.MaxAmount {
			t.Skip()
		}
		got, err := money.Parse(money.Format(minor))
		if err != nil || got != minor {
			t.Fatalf("Parse(Format(%d)) = %d, %v", minor, got, err)
		}
	})
}

func TestFormatINR(t *testing.T) {
	for _, tc := range []struct {
		minor int64
		want  string
	}{
		{0, "₹0.00"},
		{5, "₹0.05"},
		{99999, "₹999.99"},
		{100000, "₹1,000.00"},
		{200000, "₹2,000.00"},
		{1000000, "₹10,000.00"},
		{10000000, "₹1,00,000.00"},      // one lakh
		{123456789, "₹12,34,567.89"},    // twelve lakh...
		{1000000000, "₹1,00,00,000.00"}, // one crore
		{money.MaxAmount, "₹1,00,00,000.00"},
		{-50000000, "-₹5,00,000.00"}, // treasury balances are negative
	} {
		if got := money.FormatINR(tc.minor); got != tc.want {
			t.Errorf("FormatINR(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}
