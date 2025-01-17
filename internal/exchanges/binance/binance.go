package binance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

const (
	ModeIntraday = "intraday"
	ModeScalp    = "scalp"
	ModeSpot     = "spot"
)

type Client struct {
	futuresClient *futures.Client
	cache         *cache.Cache
	mu            sync.Mutex
	deltaAgg      *DeltaAggregator
	wsKline1m     *WsKlineAggregator
}

// SymbolDelta используется в выводе Top Delta
type SymbolDelta struct {
	Symbol             string  `json:"symbol"`
	BuyVolume          float64 `json:"buy_volume"`
	SellVolume         float64 `json:"sell_volume"`
	Delta              float64 `json:"delta"`
	RatioToDailyVolume float64 `json:"ratio_to_daily_volume"`
	LastTradeID        string
	CurrentPrice       float64 `json:"current_price"`
}

// NewClient создаёт фьючерсный клиент Binance
func NewClient(apiKey, apiSecret string) *Client {
	futuresCli := futures.NewClient(apiKey, apiSecret)
	cc := cache.New(5*time.Minute, 10*time.Minute)

	c := &Client{
		futuresClient: futuresCli,
		cache:         cc,
	}
	// Инициализируем DeltaAggregator (24h = 96 слотов по 15м)
	c.deltaAgg = NewDeltaAggregator(5*time.Minute, 96)
	return c
}

// DeltaAggregator — геттер
func (c *Client) DeltaAggregator() *DeltaAggregator {
	return c.deltaAgg
}

func generateCacheKey(function string, params ...string) string {
	return fmt.Sprintf("%s:%s", function, strings.Join(params, ","))
}

// Получаем список USDM Futures из кэша
func (c *Client) NewExchangeInfoService(ctx context.Context) ([]string, error) {
	cacheKey := generateCacheKey("USDMFuturesSymbols")
	if val, found := c.cache.Get(cacheKey); found {
		return val.([]string), nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := c.futuresClient.NewExchangeInfoService().Do(ctx2)
	if err != nil {
		log.Errorf("GetUSDMFuturesSymbolsCached: %v", err)
		return nil, fmt.Errorf("exchange info error: %w", err)
	}
	var symbols []string
	for _, s := range info.Symbols {
		if s.ContractType == "PERPETUAL" && s.QuoteAsset == "USDT" && s.Status == "TRADING" {
			symbols = append(symbols, s.Symbol)
		}
	}
	c.cache.Set(cacheKey, symbols, cache.DefaultExpiration)
	log.Infof("Кэш обновлён: загружено %d символов USDM Futures.", len(symbols))
	return symbols, nil
}

// Проверка, торгуется ли символ
func (c *Client) IsSymbolTrading(ctx context.Context, symbol string) (bool, error) {
	cacheKey := generateCacheKey("IsSymbolTrading", symbol)
	if v, f := c.cache.Get(cacheKey); f {
		return v.(bool), nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	info, err := c.futuresClient.NewExchangeInfoService().Do(ctx2)
	if err != nil {
		return false, fmt.Errorf("exchange info error: %w", err)
	}
	var trading bool
	for _, s := range info.Symbols {
		if s.Symbol == symbol {
			trading = (s.Status == "TRADING")
			break
		}
	}
	c.cache.Set(cacheKey, trading, 5*time.Minute)
	return trading, nil
}

// Возвращаем 24h volume
func (c *Client) Get24hVolumeCached(ctx context.Context, symbol string) (float64, error) {
	cacheKey := generateCacheKey("24hVolume", symbol)
	if v, found := c.cache.Get(cacheKey); found {
		return v.(float64), nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tickers, err := c.futuresClient.NewListPriceChangeStatsService().Symbol(symbol).Do(ctx2)
	if err != nil {
		return 0, err
	}
	if len(tickers) == 0 {
		return 0, fmt.Errorf("empty 24h stats: %s", symbol)
	}
	vol, err := strconv.ParseFloat(tickers[0].Volume, 64)
	if err != nil {
		return 0, err
	}
	c.cache.Set(cacheKey, vol, 2*time.Minute)
	return vol, nil
}

// Текущая цена (Mark Price)
func (c *Client) GetCurrentPrice(ctx context.Context, symbol string) (float64, error) {
	cacheKey := generateCacheKey("CurrentPrice", symbol)
	if p, found := c.cache.Get(cacheKey); found {
		return p.(float64), nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Если "intraday" у вас это фьючерсы:
	prices, err := c.futuresClient.NewListPricesService().Symbol(symbol).Do(ctx2)
	if err != nil || len(prices) == 0 {
		return 0, fmt.Errorf("не удалось получить цену для %s: %w", symbol, err)
	}
	price, err := strconv.ParseFloat(prices[0].Price, 64)
	if err != nil {
		return 0, fmt.Errorf("не удалось распарсить цену %s: %w", symbol, err)
	}
	c.cache.Set(cacheKey, price, cache.DefaultExpiration)
	return price, nil
}
