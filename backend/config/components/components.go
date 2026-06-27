// Package components provides a function to create a new instance of raptor.Components with all the necessary services, middlewares, and controllers for the application.
package components

import (
	"github.com/go-raptor/connectors/bun/postgres"
	"github.com/go-raptor/raptor/v4"
	"github.com/h00s/kluttr/backend/db"
)

func New() *raptor.Components {
	return &raptor.Components{
		DatabaseConnector: postgres.NewPostgresConnector(db.MigrationsFS()),
		Services:          Services(),
		Middlewares:       Middlewares(),
		Controllers:       Controllers(),
	}
}
