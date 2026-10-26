package poller

import (
	"context"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func TestReconcilePayloadValidation(t *testing.T) {
	for _, p := range []string{`{"workspace_id":"ws-1"}`, `{"watch_id":"w"}`, `{"workspace_id":"  ","watch_id":""}`, `{"workspace_id":5,"watch_id":"w"}`, `not json`, `[]`} {
		err := Reconcile(context.Background(), Deps{}, queue.FailureNotice{Payload: []byte(p)})
		if err == nil || !strings.Contains(err.Error(), "watch poll failure payload requires workspace_id and watch_id") {
			t.Errorf("payload %s: err = %v", p, err)
		}
	}
}
