package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed all:migrations
var migrationsFS embed.FS

const migrationsDir = "migrations"

// maxEmbeddedVersion returns the highest migration version embedded in
// the binary. Computed by scanning filename prefixes ("0001_*.sql" →
// 1). Used by the Migrate version gate.
func maxEmbeddedVersion() (uint, error) {
	entries, err := migrationsFS.ReadDir(migrationsDir)
	if err != nil {
		return 0, err
	}
	var maxV uint
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		underscore := strings.IndexByte(name, '_')
		if underscore <= 0 {
			continue
		}
		n, err := strconv.ParseUint(name[:underscore], 10, 64)
		if err != nil {
			continue
		}
		if uint(n) > maxV {
			maxV = uint(n)
		}
	}
	if maxV == 0 {
		return 0, errors.New("store: no migrations embedded")
	}
	return maxV, nil
}

// Migrate brings the schema up to the maximum version embedded in this
// binary. Idempotent. Returns ErrSchemaTooNew if the on-disk schema is
// already at a higher version than this binary supports.
func (s *SQLite) Migrate(ctx context.Context) error {
	src, err := iofs.New(migrationsFS, migrationsDir)
	if err != nil {
		return err
	}
	// Release iofs-source resources when Migrate returns. We
	// intentionally don't call m.Close() (defined below) because with
	// WithInstance migrate v4's Close() would close the underlying
	// *sql.DB we passed in — and that DB is owned by *SQLite, used by
	// every other store method.
	defer func() { _ = src.Close() }()

	drv, err := sqlitemigrate.WithInstance(s.db, &sqlitemigrate.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
	if err != nil {
		return err
	}

	maxV, err := maxEmbeddedVersion()
	if err != nil {
		return err
	}

	diskV, dirty, vErr := m.Version()
	switch {
	case vErr == nil:
		if dirty {
			return fmt.Errorf("store: schema_migrations is dirty at version %d; manual recovery required", diskV)
		}
		if diskV > maxV {
			return fmt.Errorf("%w: db at version %d, binary supports up to %d",
				ErrSchemaTooNew, diskV, maxV)
		}
	case errors.Is(vErr, migrate.ErrNilVersion):
		// Fresh DB. Proceed to Up().
	default:
		return vErr
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
