package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// Migrate applies River's own schema migrations (river_job, river_queue,
// river_leader, ...) via River's Go migration API.
//
// It is called from internal/migrate.Run while that holds the migration
// advisory lock, so — like our SQL migrations — at most one replica applies
// River's migrations at a time and the others observe the finished state.
// It must not be called outside that critical section.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{
		Logger: slog.Default(),
	})
	if err != nil {
		return fmt.Errorf("jobs: create river migrator: %w", err)
	}
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{})
	if err != nil {
		return fmt.Errorf("jobs: apply river migrations: %w", err)
	}
	for _, v := range res.Versions {
		slog.Info("applied River migration", "version", v.Version, "name", v.Name)
	}
	return nil
}
