package frontend

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

const MaxCollectionCards = core.MaxCollectionCards

// CollectionCardReader opens an unscoped Home collection without converting
// it to a movie or show library page.
type CollectionCardReader interface {
	OpenCollectionCard(ctx context.Context, viewer *Viewer, key RatingKey, after int64, offset, limit int) (CollectionCardPage, error)
}

type CollectionCardPage struct {
	Collection Collection
	Titles     []Title
	Revision   string
	Next       int64
	Consumed   int
	More       bool
}

func ValidRowID(id string) bool { return core.ValidRowID(id) }

// CollectionCardArtworkSubject distinguishes an authorized card image from a
// full collection poster. It is included in the signed image claim.
const CollectionCardArtworkSubject = "collection-card"

type collectionCardArtworkKey struct{}

func WithCollectionCardArtwork(ctx context.Context) context.Context {
	return context.WithValue(ctx, collectionCardArtworkKey{}, true)
}

func CollectionCardArtwork(ctx context.Context) bool {
	card, _ := ctx.Value(collectionCardArtworkKey{}).(bool)
	return card
}
