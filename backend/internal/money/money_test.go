package money_test

import (
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
