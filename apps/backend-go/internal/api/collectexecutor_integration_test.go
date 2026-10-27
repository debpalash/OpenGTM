//go:build integration

package api

import (
	"testing"
)

// plan 15 WP-COLLECT: POST /api/collect enqueues executor 'go' only for the GO_EXECUTOR_COLLECT cohort,
// always with the job's workspace on the queue row.
func TestCollectEnqueueExecutorFollowsCohort(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "python"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "ws-alpha"}, "python"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "ws-alpha", "GO_EXECUTOR_COLLECT": "1"}, "go"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "ws-beta", "GO_EXECUTOR_COLLECT": "1"}, "python"},
	} {
		for _, k := range []string{"GO_EXECUTOR_ENABLED", "GO_EXECUTOR_WORKSPACES", "GO_EXECUTOR_COLLECT", "GO_ALL_NATIVE"} {
			t.Setenv(k, c.env[k])
		}
		e := newCollectEnv(t)
		rec := e.post("alice", "ws-alpha", `{"query":"IT staffing companies in Bangalore"}`)
		if rec.Code != 200 {
			t.Fatalf("%v: %d %s", c.env, rec.Code, rec.Body)
		}
		if n := ownerCount(t, `SELECT count(*) FROM jobs WHERE type='collect' AND workspace_id='ws-alpha' AND executor=$1`, c.want); n != 1 {
			t.Errorf("%v: want one %s row, got %d", c.env, c.want, n)
		}
		e.purge(t)
	}
}
