package persistence

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	_ "github.com/lib/pq"

	"1333/internal/models"

	log "github.com/sirupsen/logrus"
)

type UserStore struct {
	db    *sql.DB
	Mu    sync.RWMutex
	Users map[int64]*models.UserSettings
}

func NewUserStorePostgres(dsn string) (*UserStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к Postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping Postgres error: %w", err)
	}

	store := &UserStore{
		db:    db,
		Users: make(map[int64]*models.UserSettings),
	}
	return store, nil
}

func (us *UserStore) Load() error {
	us.Mu.Lock()
	defer us.Mu.Unlock()

	rows, err := us.db.Query(`
        SELECT
            user_id,
            mode,
            change_threshold,
            time_frame,
            target_bot,
            monitor_oi,
            oi_threshold,
            some_intraday_metric,
            some_intraday_threshold,
            monitor_spot,
            spot_volume_multiplier,
            spot_interval,
            spot_metric,
            monitor_delta,
            delta_interval,
            symbols
        FROM user_settings
    `)
	if err != nil {
		log.Errorf("Ошибка SELECT user_settings: %v", err)
		return err
	}
	defer rows.Close()

	us.Users = make(map[int64]*models.UserSettings)

	for rows.Next() {
		var (
			uid int64
			s   models.UserSettings
			sy  string
		)
		err := rows.Scan(
			&uid,
			&s.Mode,
			&s.ChangeThreshold,
			&s.TimeFrame,
			&s.TargetBot,
			&s.MonitorOI,
			&s.OIThreshold,
			&s.SomeIntradayMetric,
			&s.SomeIntradayThreshold,
			&s.MonitorSpot,
			&s.SpotVolumeMultiplier,
			&s.SpotInterval,
			&s.SpotMetric,
			&s.MonitorDelta,
			&s.DeltaInterval,
			&sy,
		)
		if err != nil {
			log.Errorf("Ошибка чтения строки user_settings: %v", err)
			continue
		}
		if sy != "" {
			s.Symbols = strings.Split(sy, ",")
		}
		us.Users[uid] = &s
	}
	if err := rows.Err(); err != nil {
		log.Errorf("Rows error user_settings: %v", err)
	}
	log.Infof("UserStorePostgres: загрузили %d пользователей из БД", len(us.Users))
	return nil
}

func (us *UserStore) Save() error {
	us.Mu.RLock()
	defer us.Mu.RUnlock()

	for uid, s := range us.Users {
		if err := us.upsertOne(uid, s); err != nil {
			log.Errorf("Ошибка сохранения пользователя %d: %v", uid, err)
		}
	}
	log.Infof("UserStorePostgres: сохранили %d пользователей", len(us.Users))
	return nil
}

func (us *UserStore) upsertOne(uid int64, s *models.UserSettings) error {
	sy := strings.Join(s.Symbols, ",")

	_, err := us.db.Exec(`
        INSERT INTO user_settings
        (
            user_id,
            mode,
            change_threshold,
            time_frame,
            target_bot,
            monitor_oi,
            oi_threshold,
            some_intraday_metric,
            some_intraday_threshold,
            monitor_spot,
            spot_volume_multiplier,
            spot_interval,
            spot_metric,
            monitor_delta,
            delta_interval,
            symbols
        )
        VALUES
        ($1, $2, $3, $4, $5, $6, $7, $8, $9,
         $10, $11, $12, $13, $14, $15, $16
        )
        ON CONFLICT (user_id) DO UPDATE SET
            mode = EXCLUDED.mode,
            change_threshold = EXCLUDED.change_threshold,
            time_frame = EXCLUDED.time_frame,
            target_bot = EXCLUDED.target_bot,
            monitor_oi = EXCLUDED.monitor_oi,
            oi_threshold = EXCLUDED.oi_threshold,
            some_intraday_metric = EXCLUDED.some_intraday_metric,
            some_intraday_threshold = EXCLUDED.some_intraday_threshold,
            monitor_spot = EXCLUDED.monitor_spot,
            spot_volume_multiplier = EXCLUDED.spot_volume_multiplier,
            spot_interval = EXCLUDED.spot_interval,
            spot_metric = EXCLUDED.spot_metric,
            monitor_delta = EXCLUDED.monitor_delta,
            delta_interval = EXCLUDED.delta_interval,
            symbols = EXCLUDED.symbols
    `,
		uid,
		s.Mode,
		s.ChangeThreshold,
		s.TimeFrame,
		s.TargetBot,
		s.MonitorOI,
		s.OIThreshold,
		s.SomeIntradayMetric,
		s.SomeIntradayThreshold,
		s.MonitorSpot,
		s.SpotVolumeMultiplier,
		s.SpotInterval,
		s.SpotMetric,
		s.MonitorDelta,
		s.DeltaInterval,
		sy,
	)
	return err
}

func (us *UserStore) Set(userID int64, s models.UserSettings) {
	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.Users[userID] = &s
}

func (us *UserStore) All() map[int64]*models.UserSettings {
	us.Mu.RLock()
	defer us.Mu.RUnlock()

	res := make(map[int64]*models.UserSettings, len(us.Users))
	for uid, val := range us.Users {
		if val != nil {
			copyVal := *val
			res[uid] = &copyVal
		}
	}
	return res
}
