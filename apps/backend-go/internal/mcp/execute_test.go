package mcp

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// execFixture returns the recorded tool name and text payload of a tools fixture.
func execFixture(t *testing.T, file string) (name, text string) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/mcp/tools/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Request struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		} `json:"request"`
		Response struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Request.Params.Name, f.Response.Result.Content[0].Text
}

func TestUnknownToolAndCapabilityMessagesMatchFixtures(t *testing.T) {
	s := NewServer(Deps{InWorkspace: nilTxRunner, Cfg: Config{WriteEnabled: true}}, NewRegistry(nil, nil))
	c := Ctx{WorkspaceID: "w", Capabilities: map[string]bool{CapLeadsRead: true}}
	for _, f := range []string{"read__unknown_tool.json", "read__capability_not_granted_write.json", "read__capability_not_granted_admin.json"} {
		name, want := execFixture(t, f)
		if got := s.Execute(context.Background(), c, name, Args{}); got != want {
			t.Fatalf("%s: got %q want %q", f, got, want)
		}
	}
}

func TestExecuteIsWired(t *testing.T) {
	s := NewServer(Deps{InWorkspace: nilTxRunner}, NewRegistry(nil, nil))
	if s.Execute == nil {
		t.Fatal("NewServer must set Execute to the write pipeline")
	}
}
