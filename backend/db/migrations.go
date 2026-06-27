// Package db provides database access and management for the application, including handling database migrations.
package db

import (
	"embed"
	"io/fs"
)

//go:embed all:migrations
var migrationsFS embed.FS

func MigrationsFS() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}
