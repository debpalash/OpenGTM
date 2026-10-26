package api

import (
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Python iterates a string waterfall by character and a dict by key instead of failing.
func TestEstimateChainsNonListWaterfall(t *testing.T) {
	cols, err := wbcrud.ParseColumns([]byte(`[
 {"id":"a","type":"waterfall","target_field":"email","waterfall":"ab"},
 {"id":"b","type":"waterfall","target_field":"email","waterfall":{"hunter_io":1}}]`))
	if err != nil {
		t.Fatal(err)
	}
	by, order, err := estimateChains(cols)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"a", "b"}) || !reflect.DeepEqual(by["a"], []string{"a", "b"}) || !reflect.DeepEqual(by["b"], []string{"hunter_io"}) {
		t.Fatalf("%v %v", order, by)
	}
}
