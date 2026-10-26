package copilot

import (
	"context"
	"errors"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// gate is implemented in the next commit.
func (t *Turn) gate(context.Context, *ssestream.Writer, []parsedCall) error {
	return errors.New("copilot: approval gate not implemented")
}
