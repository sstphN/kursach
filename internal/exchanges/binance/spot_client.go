package binance

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/adshao/go-binance/v2"
	log "github.com/sirupsen/logrus"
)

// SpotClient хранит binance.Client для REST-запросов + агрегатор (если нужно)
type SpotClient struct {
	spotAPI   *binance.Client
	klineAggr *WsSpotKlineAggregator
}

// NewSpotClient — конструктор
func NewSpotClient(apiKey, apiSecret string) *SpotClient {
	cli := binance.NewClient(apiKey, apiSecret)
	return &SpotClient{
		spotAPI:   cli,
		klineAggr: NewWsSpotKlineAggregator(),
	}
}

// StartWebsocket — подписка на kline
func (sc *SpotClient) StartWebsocket(ctx context.Context, symbols []string, interval string) error {
	streamsMap := make(map[string]string)
	for _, sym := range symbols {
		streamsMap[strings.ToLower(sym)] = interval
	}
	doneC, stopC, err := binance.WsCombinedKlineServe(
		streamsMap,
		func(e *binance.WsKlineEvent) {
			sc.klineAggr.handleKline(e)
		},
		func(err error) {
			log.Errorf("Spot Kline WS error: %v", err)
		},
	)
	if err != nil {
		return err
	}

	go func() {
		select {
		case <-ctx.Done():
			close(stopC)
		case <-doneC:
		}
	}()
	log.Infof("Spot WebSocket kline started for %d symbols", len(symbols))
	return nil
}

// ------------------- Методы, берущие данные из WsSpotKlineAggregator -------------------

func (sc *SpotClient) GetLastVolume(symbol string) float64 {
	return sc.klineAggr.GetLastVolume(symbol)
}

func (sc *SpotClient) GetSum10Volumes(symbol string) float64 {
	return sc.klineAggr.GetSum10Volumes(symbol)
}

// ------------------- REST методы -------------------

// Возвращает объём последней закрытой свечи за указанный interval (в USD)
func (sc *SpotClient) GetTradingVolumeUSD(ctx context.Context, symbol, interval string) (float64, error) {
	// Получаем последнюю свечу
	klines, err := sc.spotAPI.NewKlinesService().
		Symbol(symbol).
		Interval(interval).
		Limit(1).
		Do(ctx)
	if err != nil {
		return 0, err
	}
	if len(klines) == 0 {
		return 0, fmt.Errorf("нет данных по свечам для символа %s", symbol)
	}
	// Парсим объем (Volume — количество монет за свечу)
	baseVol, err := strconv.ParseFloat(klines[0].Volume, 64)
	if err != nil {
		return 0, err
	}

	// Получаем текущую цену (в USDT), вызовем наш новый метод:
	price, err := sc.GetCurrentPrice(ctx, symbol)
	if err != nil {
		return 0, err
	}
	// Умножаем (base asset qty * price)
	return baseVol * price, nil
}

// GetAverageVolume — средний объём за `periods` последних свечей (пока что base volume)
func (sc *SpotClient) GetAverageVolume(ctx context.Context, symbol, interval string, periods int) (float64, error) {
	if periods <= 0 {
		return 0, nil
	}
	klines, err := sc.spotAPI.NewKlinesService().
		Symbol(symbol).
		Interval(interval).
		Limit(periods).
		Do(ctx)
	if err != nil {
		return 0, err
	}
	if len(klines) == 0 {
		return 0, nil
	}
	var sum float64
	for _, k := range klines {
		vol, errParse := strconv.ParseFloat(k.Volume, 64)
		if errParse != nil {
			return 0, errParse
		}
		sum += vol
	}
	avg := sum / float64(len(klines))
	return avg, nil
}

// GetCurrentPrice — текущая цена (Spot)
func (sc *SpotClient) GetCurrentPrice(ctx context.Context, symbol string) (float64, error) {
	// Пример: /api/v3/ticker/price
	// или NewListPricesService().Symbol(symbol)
	prices, err := sc.spotAPI.NewListPricesService().
		Symbol(symbol).
		Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("не удалось получить цену для %s: %w", symbol, err)
	}
	if len(prices) == 0 {
		return 0, fmt.Errorf("цена для %s не найдена", symbol)
	}
	price, err := strconv.ParseFloat(prices[0].Price, 64)
	if err != nil {
		return 0, fmt.Errorf("не удалось преобразовать цену для %s: %w", symbol, err)
	}
	return price, nil
}
