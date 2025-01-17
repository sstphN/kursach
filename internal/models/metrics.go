package models

// MetricID — перечисляем метрики:
type MetricID string

const (
	MetricScalpPD       MetricID = "scalp_pd"
	MetricIntradayOI    MetricID = "intraday_oi"
	MetricIntradayDelta MetricID = "intraday_delta"
	MetricIntradayPD    MetricID = "intraday_pd"
	MetricSpotVolume    MetricID = "spot_volume"
)

// MetricSettings — настройки конкретной метрики
type MetricSettings struct {
	Enabled   bool     `json:"enabled"`
	TargetBot string   `json:"target_bot"`
	TimeFrame string   `json:"timeframe"`
	Threshold float64  `json:"threshold"`
	Symbols   []string `json:"symbols,omitempty"`
}

// UserConfig — объединяет все метрики одного пользователя
type UserConfig struct {
	UserID  int64                        `json:"user_id"`
	Metrics map[MetricID]*MetricSettings `json:"metrics"`
}
