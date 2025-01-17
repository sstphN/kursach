package binance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/adshao/go-binance/v2/futures"
	log "github.com/sirupsen/logrus"
)

// WsKlineAggregator хранит последнюю цену по символу (обновляется из kline)
type WsKlineAggregator struct {
	mu        sync.RWMutex
	lastPrice map[string]float64
}

// StartWebsocketStreams запускает WS для aggTrade + Kline
func (c *Client) StartWebsocketStreams(ctx context.Context) error {
	symbols, err := c.NewExchangeInfoService(ctx)
	if err != nil {
		return err
	}

	if c.deltaAgg != nil {
		c.deltaAgg.Start(ctx) // запускаем ротацию слотов для дельты
	}

	c.wsKline1m = &WsKlineAggregator{
		lastPrice: make(map[string]float64),
	}

	// Подписка на aggTrade
	if err := c.WsCombinedAggTradeServe(ctx, symbols); err != nil {
		return err
	}
	// Подписка на kline 1m
	if err := c.startKlineStreams(ctx, symbols, "1m"); err != nil {
		return err
	}
	return nil
}

// WsCombinedAggTradeServe — подписка на @aggTrade
func (c *Client) WsCombinedAggTradeServe(ctx context.Context, symbols []string) error {
	chunks := chunkSymbols(symbols, 200)
	for _, chunk := range chunks {
		var streams []string
		for _, sym := range chunk {
			streams = append(streams, fmt.Sprintf("%s@aggTrade", strings.ToLower(sym)))
		}
		go func(streamsSlice []string) {
			doneC, stopC, err := futures.WsCombinedAggTradeServe(
				streamsSlice,
				func(e *futures.WsAggTradeEvent) {
					// Добавили парсинг цены:
					q, _ := strconv.ParseFloat(e.Quantity, 64)
					p, _ := strconv.ParseFloat(e.Price, 64)
					volumeUsd := q * p

					isBuy := !e.Maker
					if c.deltaAgg != nil {
						c.deltaAgg.AddTrade(e.Symbol, isBuy, volumeUsd)
					}
				},
				func(err error) {
					log.Errorf("aggTrade WS error: %v", err)
				},
			)
			if err != nil {
				log.Errorf("startAggTradeStreams: %v", err)
				return
			}
			select {
			case <-ctx.Done():
				close(stopC)
			case <-doneC:
			}
		}(streams)
	}
	return nil
}

func (c *Client) startKlineStreams(ctx context.Context, symbols []string, interval string) error {
	chunks := chunkSymbols(symbols, 200)
	for _, chunk := range chunks {
		streamsMap := make(map[string]string)
		for _, sym := range chunk {
			streamsMap[strings.ToLower(sym)] = interval
		}
		go func(sm map[string]string) {
			doneC, stopC, err := futures.WsCombinedKlineServe(
				sm,
				func(e *futures.WsKlineEvent) {
					c.wsKline1m.handleKline(e)
				},
				func(err error) {
					log.Errorf("kline WS error: %v", err)
				},
			)
			if err != nil {
				log.Errorf("startKlineStreams: %v", err)
				return
			}
			select {
			case <-ctx.Done():
				close(stopC)
			case <-doneC:
			}
		}(streamsMap)
	}
	return nil
}

func (w *WsKlineAggregator) handleKline(e *futures.WsKlineEvent) {
	if !e.Kline.IsFinal {
		return
	}
	price, _ := strconv.ParseFloat(e.Kline.Close, 64)
	w.mu.Lock()
	w.lastPrice[e.Symbol] = price
	w.mu.Unlock()
}

func chunkSymbols(symbols []string, size int) [][]string {
	var res [][]string
	for i := 0; i < len(symbols); i += size {
		end := i + size
		if end > len(symbols) {
			end = len(symbols)
		}
		res = append(res, symbols[i:end])
	}
	return res
}
