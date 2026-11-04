package livetv

import (
	"testing"
	"time"
)

func TestDedupeProgrammesKeepsLast(t *testing.T) {
	at := time.Unix(100, 0)
	got := dedupeProgrammes([]Programme{
		{ChannelID: "a", Start: at, Item: Item{Title: "first"}},
		{ChannelID: "a", Start: at, Item: Item{Title: "second"}},
		{ChannelID: "a", Start: at.Add(time.Hour), Item: Item{Title: "later"}},
		{ChannelID: "b", Start: at, Item: Item{Title: "other"}},
	})
	if len(got) != 3 {
		t.Fatalf("got %d programmes: %+v", len(got), got)
	}
	if got[0].Item.Title != "second" || got[1].Item.Title != "later" || got[2].Item.Title != "other" {
		t.Fatalf("unexpected order: %+v", got)
	}
}
