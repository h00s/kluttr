// Package components provides a function to create a new instance of raptor.Components with all the necessary services, middlewares, and controllers for the application.
package components

import (
	"github.com/go-raptor/raptor/v4"
)

func New() *raptor.Components {
	return &raptor.Components{
		Services:    Services(),
		Middlewares: Middlewares(),
		Controllers: Controllers(),
	}
}
