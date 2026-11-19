package demoseed_test

import (
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/demoseed"
)

func TestSampleIdentity(t *testing.T) {
	if !strings.HasPrefix(demoseed.SampleName, "Sample") {
		t.Fatalf("sample name %q must say it is a sample", demoseed.SampleName)
	}
	a, b := demoseed.SampleWorkbookID("ws-a"), demoseed.SampleWorkbookID("ws-b")
	if a == b || a != demoseed.SampleWorkbookID("ws-a") || len(a) > 36 {
		t.Fatalf("ids must be stable, per workspace and fit varchar(36): %q %q", a, b)
	}
	if a == demoseed.WorkbookID {
		t.Fatal("sample id collides with the CLI demo id")
	}
}
