// persistence/user_metrics_postgres.go

package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"1333/internal/models"

	_ "github.com/lib/pq"
	log "github.com/sirupsen/logrus"
)

// UserMetricsStore — хранилище для user_metrics (таблица)
type UserMetricsStore struct {
	db       *sql.DB
	mu       sync.RWMutex
	userData map[int64]*models.UserConfig
}

// NewUserMetricsStorePostgres — создаём Store
func NewUserMetricsStorePostgres(dsn string) (*UserMetricsStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к Postgres (metrics): %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping Postgres error (metrics): %w", err)
	}

	ums := &UserMetricsStore{
		db:       db,
		userData: make(map[int64]*models.UserConfig),
	}
	return ums, nil
}

// LoadAll — читает ВСЕ user_metrics из таблицы
func (ums *UserMetricsStore) LoadAll(ctx context.Context) error {
	ums.mu.Lock()
	defer ums.mu.Unlock()

	rows, err := ums.db.QueryContext(ctx, `
        SELECT 
          user_id,
          metric_id,
          enabled,
          target_bot,
          timeframe,
          threshold,
          symbols
        FROM user_metrics
    `)
	if err != nil {
		return fmt.Errorf("LoadAll user_metrics: %w", err)
	}
	defer rows.Close()

	ums.userData = make(map[int64]*models.UserConfig)

	for rows.Next() {
		var (
			userID    int64
			metricID  string
			enabled   bool
			targetBot string
			tf        string
			th        float64
			sy        string
		)
		if err := rows.Scan(&userID, &metricID, &enabled, &targetBot, &tf, &th, &sy); err != nil {
			log.Errorf("LoadAll user_metrics scan error: %v", err)
			continue
		}

		uc, ok := ums.userData[userID]
		if !ok {
			uc = &models.UserConfig{
				UserID:  userID,
				Metrics: make(map[models.MetricID]*models.MetricSettings),
			}
			ums.userData[userID] = uc
		}

		ms := &models.MetricSettings{
			Enabled:   enabled,
			TargetBot: targetBot,
			TimeFrame: tf,
			Threshold: th,
		}
		if sy != "" {
			ms.Symbols = strings.Split(sy, ",")
		}

		uc.Metrics[models.MetricID(metricID)] = ms
	}
	if err := rows.Err(); err != nil {
		return err
	}

	log.Infof("LoadAll: загружено %d пользователей (user_metrics)", len(ums.userData))
	return nil
}

// SaveAll — сохраняем все userConfigs в таблицу user_metrics
func (ums *UserMetricsStore) SaveAll(ctx context.Context) error {
	ums.mu.RLock()
	defer ums.mu.RUnlock()

	for userID, uc := range ums.userData {
		for mid, ms := range uc.Metrics {
			if err := ums.upsertOne(ctx, userID, mid, ms); err != nil {
				log.Errorf("SaveAll: upsertOne user=%d metric=%s err=%v", userID, mid, err)
			}
		}
	}
	return nil
}

// upsertOne — вставляем/обновляем одну запись
func (ums *UserMetricsStore) upsertOne(ctx context.Context, userID int64, m models.MetricID, ms *models.MetricSettings) error {
	sy := ""
	if len(ms.Symbols) > 0 {
		sy = strings.Join(ms.Symbols, ",")
	}
	_, err := ums.db.ExecContext(ctx, `
        INSERT INTO user_metrics
        (user_id, metric_id, enabled, target_bot, timeframe, threshold, symbols)
        VALUES
        ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (user_id, metric_id)
        DO UPDATE SET
          enabled = EXCLUDED.enabled,
          target_bot = EXCLUDED.target_bot,
          timeframe = EXCLUDED.timeframe,
          threshold = EXCLUDED.threshold,
          symbols = EXCLUDED.symbols
    `,
		userID, string(m),
		ms.Enabled,
		ms.TargetBot,
		ms.TimeFrame,
		ms.Threshold,
		sy,
	)
	return err
}

// GetUserConfig — получить (или создать) UserConfig из памяти
func (ums *UserMetricsStore) GetUserConfig(userID int64) *models.UserConfig {
	ums.mu.RLock()
	uc, ok := ums.userData[userID]
	ums.mu.RUnlock()

	if ok {
		return uc
	}

	newUC := &models.UserConfig{
		UserID:  userID,
		Metrics: make(map[models.MetricID]*models.MetricSettings),
	}
	ums.mu.Lock()
	ums.userData[userID] = newUC
	ums.mu.Unlock()

	return newUC
}

// SetMetricSettings — упрощённый helper: установить/обновить метрику для юзера
func (ums *UserMetricsStore) SetMetricSettings(ctx context.Context, userID int64, metricID models.MetricID, ms models.MetricSettings) error {
	ums.mu.Lock()
	uc, ok := ums.userData[userID]
	if !ok {
		uc = &models.UserConfig{
			UserID:  userID,
			Metrics: make(map[models.MetricID]*models.MetricSettings),
		}
		ums.userData[userID] = uc
	}
	newMS := ms
	uc.Metrics[metricID] = &newMS
	ums.mu.Unlock()

	return ums.upsertOne(ctx, userID, metricID, &newMS)
}

// AllConfigs — вернуть копию всех userConfig
func (ums *UserMetricsStore) AllConfigs() map[int64]*models.UserConfig {
	ums.mu.RLock()
	defer ums.mu.RUnlock()

	out := make(map[int64]*models.UserConfig, len(ums.userData))
	for uid, uc := range ums.userData {
		copyUC := &models.UserConfig{
			UserID:  uc.UserID,
			Metrics: make(map[models.MetricID]*models.MetricSettings),
		}
		for mID, ms := range uc.Metrics {
			msCopy := *ms
			copyUC.Metrics[mID] = &msCopy
		}
		out[uid] = copyUC
	}
	return out
}
