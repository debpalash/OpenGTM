package manifest_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

func TestProjectVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.ProjectValue) < 60 || len(v.ProjectResp) < 8 {
		t.Fatalf("too few vectors: %d/%d", len(v.ProjectValue), len(v.ProjectResp))
	}
	for _, c := range v.ProjectValue {
		sameValue(t, "project_value/"+c.Name, manifest.ProjectValue(dec(t, c.Data), c.Expr), c.Expected)
	}
	for _, c := range v.ProjectResp {
		sameValue(t, "project_response/"+c.Name, manifest.Project(dec(t, c.Data), c.Mappings), c.Expected)
	}
}
