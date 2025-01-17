package models

type UserSettings struct {
	Mode                  string    `json:"mode"`
	ChangeThreshold       float64   `json:"change_threshold"`
	TimeFrame             string    `json:"time_frame"`
	TargetBot             string    `json:"target_bot"`
	MonitorOI             bool      `json:"monitor_oi"`
	OIThreshold           float64   `json:"oi_threshold"`
	OITimeFrames          []string  `json:"oi_time_frames,omitempty"`
	SomeIntradayMetric    string    `json:"some_intraday_metric,omitempty"`
	SomeIntradayThreshold float64   `json:"some_intraday_threshold,omitempty"`
	MonitorSpot           bool      `json:"monitor_spot"`
	SpotVolumeMultiplier  float64   `json:"spot_volume_multiplier,omitempty"`
	SpotInterval          string    `json:"spot_interval,omitempty"`
	SpotMetric            string    `json:"spot_metric,omitempty"`
	Symbols               []string  `json:"symbols,omitempty"`
	OIThresholds          []float64 `json:"oi_thresholds,omitempty"`
	MonitorDelta          bool      `json:"monitor_delta"`
	DeltaInterval         string    `json:"delta_interval"`
}
