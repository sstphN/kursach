package utils

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
)

// FormatNumber — K, M, B, T
func FormatNumber(num float64) string {
	switch {
	case num >= 1e12:
		return fmt.Sprintf("%.2fT", num/1e12)
	case num >= 1e9:
		return fmt.Sprintf("%.2fB", num/1e9)
	case num >= 1e6:
		return fmt.Sprintf("%.2fM", num/1e6)
	case num >= 1e3:
		return fmt.Sprintf("%.2fK", num/1e3)
	default:
		return fmt.Sprintf("%.2f", num)
	}
}

// FormatVolume — вернём "$" + укороченный формат
func FormatVolume(volume float64) string {
	formatted := FormatNumber(volume)
	return fmt.Sprintf("$%s", formatted)
}

func FormatPrice(price float64, decimalPlaces int) string {
	format := fmt.Sprintf("%%.%df", decimalPlaces)
	formatted := fmt.Sprintf(format, price)
	formatted = strings.TrimRight(formatted, "0")
	if strings.HasSuffix(formatted, ".") {
		formatted = strings.TrimSuffix(formatted, ".")
	}
	return formatted
}

// FormatPriceWithComma — чтобы были запятые (напр. 10,000.12)
func FormatPriceWithComma(price float64, decimalPlaces int) string {
	formatted := FormatPrice(price, decimalPlaces)
	parts := strings.Split(formatted, ".")
	intPart := parts[0]
	var decPart string
	if len(parts) > 1 {
		decPart = parts[1]
	}
	var sign string
	if strings.HasPrefix(intPart, "-") {
		sign = "-"
		intPart = strings.TrimPrefix(intPart, "-")
	}
	intF, err := strconv.ParseFloat(intPart, 64)
	if err != nil {
		intF = 0
	}
	intWithCommas := humanize.Commaf(math.Abs(intF))
	if decPart != "" {
		return fmt.Sprintf("%s%s.%s", sign, intWithCommas, decPart)
	}
	return fmt.Sprintf("%s%s", sign, intWithCommas)
}
