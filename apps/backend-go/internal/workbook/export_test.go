package workbook

// SetAfterReserveHook installs the between-reserve-and-dispatch test seam; it returns a restore func.
func SetAfterReserveHook(f func()) func() {
	prev := afterReserveHook
	afterReserveHook = f
	return func() { afterReserveHook = prev }
}
