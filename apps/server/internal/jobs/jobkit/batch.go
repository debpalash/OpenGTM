package jobkit

import "context"

// KeysetPages walks a table in id order, a bounded page at a time, the way
// the Python handlers do with `id > last ORDER BY id LIMIT n`: it holds no
// cursor across the callback, so each page may commit, delete or otherwise
// change the rows already seen. fetch returns up to limit rows with id >
// afterID in ascending order; each receives every non-empty page.
func KeysetPages[T any](ctx context.Context, limit int, fetch func(ctx context.Context, afterID int64, limit int) ([]T, error), id func(T) int64, each func(page []T) error) error {
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := fetch(ctx, after, limit)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		after = id(page[len(page)-1])
		if err := each(page); err != nil {
			return err
		}
	}
}
