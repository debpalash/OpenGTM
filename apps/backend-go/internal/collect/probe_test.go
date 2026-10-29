package collect

import (
	"context"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

type slowWeb struct{ fakeWeb }

func (s *slowWeb) Search(ctx context.Context, q string, n int) []leadweb.Hit {
	<-ctx.Done()
	return nil
}

func TestNativeProber(t *testing.T) {
	names, _ := NativeProber{}.SourceList(context.Background(), time.Second)
	if len(names) != len(registry) {
		t.Fatalf("source list %d want %d (disabled sources included)", len(names), len(registry))
	}
	p := NativeProber{Web: &fakeWeb{w: loadWorld(t)}}
	got, err := p.SourceProbe(context.Background(), []string{"indiamart", "nope"}, 5*time.Second)
	if err != nil || got["indiamart"] == nil || *got["indiamart"] != 8 || got["nope"] != nil {
		t.Fatalf("probe = %v %v", got, err)
	}
	got, _ = NativeProber{Web: &slowWeb{}}.SourceProbe(context.Background(), []string{"indiamart"}, 50*time.Millisecond)
	if got["indiamart"] != nil {
		t.Fatalf("an unfinished probe must be nil, got %v", *got["indiamart"])
	}
}
