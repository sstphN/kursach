package binance

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

func (c *Client) GetChangePercent(ctx context.Context, symbol, timeframe string) (float64, float64, error) {
	cacheKey := generateCacheKey("ChangePercent", symbol, timeframe)
	if change, found := c.cache.Get(cacheKey); found {
		cp := change.(struct {
			PrevClose float64
			CurrClose float64
		})
		return cp.PrevClose, cp.CurrClose, nil
	}

	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	klines, err := c.futuresClient.NewKlinesService().
		Symbol(symbol).
		Interval(timeframe).
		Limit(2).
		Do(ctx2)
	if err != nil {
		log.Errorf("Не удалось получить klines %s: %v", symbol, err)
		return 0, 0, fmt.Errorf("не удалось получить klines: %w", err)
	}
	if len(klines) < 2 {
		log.Warnf("Недостаточно klines для %s", symbol)
		return 0, 0, fmt.Errorf("недостаточно klines")
	}

	prevClose, err := strconv.ParseFloat(klines[0].Close, 64)
	if err != nil {
		return 0, 0, err
	}
	currClose, err := strconv.ParseFloat(klines[1].Close, 64)
	if err != nil {
		return 0, 0, err
	}
	c.cache.Set(cacheKey, struct {
		PrevClose float64
		CurrClose float64
	}{prevClose, currClose}, cache.DefaultExpiration)

	return prevClose, currClose, nil
}
