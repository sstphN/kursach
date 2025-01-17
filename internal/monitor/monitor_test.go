package monitor

import (
	"testing"
)

// Тестируем parseMinutes(tf string)
func TestParseMinutes_Valid(t *testing.T) {
	tests := []struct {
		tf       string
		expected int
	}{
		{"1m", 1},
		{"5m", 5},
		{"30m", 30},
		{"1h", 60},
		{"4h", 240},
		{"1d", 1440},
	}
	for _, tt := range tests {
		got, err := parseMinutes(tt.tf)
		if err != nil {
			t.Errorf("parseMinutes(%q) вернул ошибку, а не ожидалось: %v", tt.tf, err)
			continue
		}
		if got != tt.expected {
			t.Errorf("parseMinutes(%q) = %d, ожидалось %d", tt.tf, got, tt.expected)
		}
	}
}

func TestParseMinutes_Invalid(t *testing.T) {
	invalidTF := []string{"2h", "abc", "60min", "20d"}
	for _, tf := range invalidTF {
		_, err := parseMinutes(tf)
		if err == nil {
			t.Errorf("parseMinutes(%q) ожидали ошибку, а её нет", tf)
		}
	}
}

// Тестируем hasSignificantChange(current, previous, threshold)
func TestHasSignificantChange(t *testing.T) {
	tests := []struct {
		current   float64
		previous  float64
		threshold float64
		want      bool
	}{
		// Пример: текущее = 110, предыдущее = 100 => рост на 10%
		// если threshold=5 => хотим true, threshold=15 => false
		{current: 110, previous: 100, threshold: 5, want: true},   // +10% vs 5%
		{current: 110, previous: 100, threshold: 15, want: false}, // +10% vs 15%
		// Пример: текущее=90, previous=100 => падение ~ -10%
		// threshold=5 => true, threshold=12 => false
		{current: 90, previous: 100, threshold: 5, want: true},
		{current: 90, previous: 100, threshold: 12, want: false},
		// Если previous=0 => возвращаем false
		{current: 100, previous: 0, threshold: 10, want: false},
	}
	for _, tt := range tests {
		got := hasSignificantChange(tt.current, tt.previous, tt.threshold)
		if got != tt.want {
			t.Errorf("hasSignificantChange(%.1f, %.1f, %.1f) = %v, ожидали %v",
				tt.current, tt.previous, tt.threshold, got, tt.want)
		}
	}
}
