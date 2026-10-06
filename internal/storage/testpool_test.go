package storage

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
)

func parseTestPool(url, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	return pgxpool.NewWithConfig(context.Background(), cfg)
}
