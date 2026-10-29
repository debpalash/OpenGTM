package config

import (
	"reflect"
	"testing"
)

func TestOutboundSecretRefPrefixes(t *testing.T) {
	if g := OutboundSecretRefPrefixes(func(string) string { return "" }); !reflect.DeepEqual(g, []string{"WEBHOOK_", "AUTOMATION_", "DEST_", "SLACK_"}) {
		t.Fatal(g)
	}
	if g := OutboundSecretRefPrefixes(func(string) string { return " A_, ,B_ " }); !reflect.DeepEqual(g, []string{"A_", "B_"}) {
		t.Fatal(g)
	}
}
