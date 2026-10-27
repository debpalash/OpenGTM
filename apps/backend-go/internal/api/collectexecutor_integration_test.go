//go:build integration

package api

import (
	"testing"
)

// POST /api/collect enqueues executor 'go' whatever the retired GO_EXECUTOR_COLLECT cohort says (plan 15
// cutover), always with the job's workspace on the queue row.
func TestCollectEnqueueGoIgnoringRetiredCohort(t *testing.T) {
	e := newCollectEnv(t)
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "go"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "ws-alpha"}, "go"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "ws-alpha", "GO_EXECUTOR_COLLECT": "1"}, "go"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "ws-beta", "GO_EXECUTOR_COLLECT": "1"}, "go"},
	} {
		for _, k := range []string{"GO_EXECUTOR_ENABLED", "GO_EXECUTOR_WORKSPACES", "GO_EXECUTOR_COLLECT", "GO_ALL_NATIVE"} {
			t.Setenv(k, c.env[k])
		}
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
