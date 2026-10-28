package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/config"

// goExecutor is the executor of every job the API enqueues: "go", the only executor since the plan 15 cutover.
func goExecutor(string) string { return config.ExecutorGo }
