package schedboot

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// activeWithKeyPrefix reports whether a pending or processing job of jobType has a fire key made of
// prefix plus one occurrence (shape is a regexp over the remainder). Python used LIKE 'prefix%', where
// '_' and '%' in a workspace or object id are wildcards and an id that extends another matches its keys;
// the exact prefix and the occurrence shape remove both.
func activeWithKeyPrefix(ctx context.Context, tx pgx.Tx, jobType, prefix, shape string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE type = $1 AND status IN ('pending', 'processing')
	    AND starts_with(fire_key, $2) AND substr(fire_key, char_length($2) + 1) ~ $3)`, jobType, prefix, shape).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("schedboot: active %s lookup: %w", jobType, err)
	}
	return found, nil
}

const (
	// shapeISOOrBootstrap is audiences' and playbooks' occurrence: an ISO instant or the bootstrap marker.
	shapeISOOrBootstrap = `^(bootstrap$|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?([+-][0-9]{2}:[0-9]{2})?$)`
	shapeDate           = `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`
)
