package remote

import (
	"github.com/braedonsaunders/ether/internal/plugins"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollectionInputErrorIsClientError(t *testing.T) {
	err := mapHostErr(&plugins.CollectionInputError{Message: "Use a catalogue source for uncached titles."})
	response := httptest.NewRecorder()
	if (&HostHandler{}).finishErr(response, err) {
		t.Fatal("invalid write accepted")
	}
	if response.Code != 400 || !strings.Contains(response.Body.String(), "catalogue source") {
		t.Fatalf("bad response: %d %s", response.Code, response.Body.String())
	}
}
