package plugins

import (
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollectionValidationSurvivesHostedBoundary(t *testing.T) {
	input := &CollectionInputError{Message: "Use a catalogue source for titles that are not cached."}
	mapped := mapHostError(input)
	fault, ok := mapped.(*wasm.Fault)
	if !ok || fault.Code != string(wasm.StatusInvalidInput) {
		t.Fatalf("bad fault: %#v", mapped)
	}
	manager := &Manager{}
	for _, err := range []error{input, &wasm.Error{Status: wasm.StatusInvalidInput, Message: fault.Message}} {
		response := httptest.NewRecorder()
		manager.fail(response, err)
		if response.Code != 400 || !strings.Contains(response.Body.String(), input.Message) {
			t.Fatalf("bad response: %d %s", response.Code, response.Body.String())
		}
	}
}
