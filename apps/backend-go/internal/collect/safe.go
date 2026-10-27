package collect

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
)

// goSafe runs fn in a goroutine counted by wg. A panic inside fn is recovered, logged with its
// stack and handed to onPanic (nil: ignored) as that item's error, so one hostile page or source
// fails only its own item — never the worker process and every tenant's in-flight jobs. Python's
// asyncio.gather / wait_for isolated these failures the same way.
func goSafe(wg *sync.WaitGroup, what string, fn func(), onPanic func(err error)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("%s crashed: %v", what, r)
				slog.Error("collect: recovered panic", "item", what, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				if onPanic != nil {
					onPanic(err)
				}
			}
		}()
		fn()
	}()
}
