package binance

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

// GetOpenInterest возвращает текущий OI
func (c *Client) GetOpenInterest(ctx context.Context, symbol string) (float64, error) {
	cacheKey := generateCacheKey("OpenInterest", symbol)
	if oiVal, found := c.cache.Get(cacheKey); found {
		return oiVal.(float64), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	oi, err := c.futuresClient.NewGetOpenInterestService().Symbol(symbol).Do(ctx)
	if err != nil {
		log.Errorf("Не удалось получить open interest для %s: %v", symbol, err)
		return 0, fmt.Errorf("не удалось получить open interest для %s: %w", symbol, err)
	}

	oiValue, err := strconv.ParseFloat(oi.OpenInterest, 64)
	if err != nil {
		log.Errorf("Не удалось распарсить OI для %s: %v", symbol, err)
		return 0, fmt.Errorf("не удалось распарсить OI для %s: %w", symbol, err)
	}

	c.cache.Set(cacheKey, oiValue, cache.DefaultExpiration)
	return oiValue, nil
}
