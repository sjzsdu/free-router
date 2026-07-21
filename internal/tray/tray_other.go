//go:build !darwin

package tray

import (
	"context"
	"errors"

	"github.com/sjzsdu/free-router/internal/service"
)

func Run(_ context.Context, _ *service.Manager, _, _ string) error {
	return errors.New("the menu bar is currently available on macOS only")
}
