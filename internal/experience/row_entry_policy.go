package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RowEntryPolicy shares admission between rows and persisted row interactions.
type RowEntryPolicy struct {
	Visibility          Visibility
	Favorite            func(context.Context, *core.User, FavoriteSubject) bool
	Playlist            func(context.Context, *core.User, core.RatingKey) bool
	Collections         core.Collections
	ChannelPermissions  func(context.Context, *core.User, []string) (map[string]bool, error)
	PlaylistExistsBatch func(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
}

type collectionCardHeaders interface {
	CollectionCardHeaders(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]*core.Collection, error)
}

func readCollectionCard(ctx context.Context, checker collectionCardHeaders, viewer *core.User, key core.RatingKey) *core.Collection {
	cards, err := checker.CollectionCardHeaders(ctx, viewer, []core.RatingKey{key})
	if err != nil {
		return nil
	}
	return cards[key]
}

type rowEntryBatchKey struct{}
type rowEntryBatchProof struct {
	viewer       *core.User
	cols         map[core.RatingKey]*core.Collection
	checkedCols  map[core.RatingKey]bool
	cards        map[core.RatingKey]*core.Collection
	cardsChecked bool
	channels     map[string]bool
	playlists    map[core.RatingKey]bool
	checkedPls   map[core.RatingKey]bool
}

// PrepareRows batches collection visibility and playlist ownership checks for
// one row-filtering pass. Callers invoke it fresh before each pass rather than
// caching across layout or retries.
func (p RowEntryPolicy) PrepareRows(ctx context.Context, u *core.User, rows []frontend.Row) context.Context {
	if u == nil || len(rows) == 0 {
		return ctx
	}
	var colKeys, plKeys, cardKeys []core.RatingKey
	var channelIDs []string
	checkedCols := map[core.RatingKey]bool{}
	checkedPls := map[core.RatingKey]bool{}
	addCol := func(k core.RatingKey) {
		if k != 0 && !checkedCols[k] {
			checkedCols[k] = true
			colKeys = append(colKeys, k)
		}
	}
	for _, row := range rows {
		addCol(row.Collection)
		for _, e := range row.Entries {
			switch e.Kind {
			case frontend.RowEntryCollection:
				if e.Collection != nil {
					cardKeys = append(cardKeys, e.Collection.Key)
				}
			case frontend.RowEntryLiveChannel:
				if e.LiveChannel != nil {
					channelIDs = append(channelIDs, e.LiveChannel.ID)
				}
			case frontend.RowEntryPlaylist:
				if e.Playlist != nil && !checkedPls[e.Playlist.Key] {
					checkedPls[e.Playlist.Key] = true
					plKeys = append(plKeys, e.Playlist.Key)
				}
			}
		}
	}
	proof := rowEntryBatchProof{viewer: u}
	if p.ChannelPermissions != nil && len(channelIDs) > 0 {
		proof.channels, _ = p.ChannelPermissions(ctx, u, channelIDs)
	}
	if checker, ok := p.Collections.(collectionCardHeaders); ok {
		if len(cardKeys) > 0 {
			proof.cards, _ = checker.CollectionCardHeaders(ctx, u, cardKeys)
			proof.cardsChecked = true
		}
	} else {
		for _, key := range cardKeys {
			addCol(key)
		}
	}
	if checker, ok := p.Collections.(RowViewCollectionBatchChecker); ok && len(colKeys) > 0 {
		if cols, _, err := checker.CheckMembersBatch(ctx, u, colKeys, nil, 0); err == nil {
			proof.cols = cols
			proof.checkedCols = checkedCols
		}
	}
	if p.PlaylistExistsBatch != nil && len(plKeys) > 0 {
		if pls, err := p.PlaylistExistsBatch(ctx, u.ID, plKeys); err == nil {
			proof.playlists = pls
			proof.checkedPls = checkedPls
		}
	}
	if proof.checkedCols == nil && proof.checkedPls == nil && proof.channels == nil && !proof.cardsChecked {
		return ctx
	}
	return context.WithValue(ctx, rowEntryBatchKey{}, proof)
}

func (p RowEntryPolicy) Subject(ctx context.Context, u *core.User, kind, id string) bool {
	switch frontend.RowEntryKind(kind) {
	case frontend.RowEntryTitle:
		return p.Favorite(ctx, u, FavoriteSubject{Kind: FavoriteKindTitle, ID: id})
	case frontend.RowEntryCollection:
		if proof, ok := ctx.Value(rowEntryBatchKey{}).(rowEntryBatchProof); ok && proof.viewer == u {
			if key, err := core.ParseRatingKey(id); err == nil && proof.checkedCols[key] {
				c := proof.cols[key]
				return c != nil && p.Visibility.CollectionAllowed(u, c)
			}
		}
		return p.Favorite(ctx, u, FavoriteSubject{Kind: FavoriteKindCollection, ID: id})
	case frontend.RowEntryLiveChannel:
		if proof, ok := ctx.Value(rowEntryBatchKey{}).(rowEntryBatchProof); ok && proof.viewer == u && proof.channels != nil {
			return proof.channels[id]
		}
		return p.Favorite(ctx, u, FavoriteSubject{Kind: FavoriteKindChannel, ID: id})
	case frontend.RowEntryPlaylist:
		key, err := core.ParseRatingKey(id)
		if err != nil {
			return false
		}
		if proof, ok := ctx.Value(rowEntryBatchKey{}).(rowEntryBatchProof); ok && proof.viewer == u && proof.checkedPls[key] {
			return proof.playlists[key]
		}
		return p.Playlist(ctx, u, key)
	default:
		return false
	}
}
func (p RowEntryPolicy) Row(ctx context.Context, u *core.User, row frontend.Row) bool {
	return row.Collection == 0 || p.Subject(ctx, u, string(frontend.RowEntryCollection), row.Collection.String())
}
func (p RowEntryPolicy) Entry(ctx context.Context, u *core.User, e frontend.RowEntry) bool {
	switch e.Kind {
	case frontend.RowEntryTitle:
		return e.Title != nil && p.Visibility.RowTitle(ctx, u, e.Title, e.Watchlist)
	case frontend.RowEntryCollection:
		if e.Collection == nil {
			return false
		}
		if checker, ok := p.Collections.(collectionCardHeaders); ok {
			var card *core.Collection
			if proof, ok := ctx.Value(rowEntryBatchKey{}).(rowEntryBatchProof); ok && proof.viewer == u && proof.cardsChecked {
				card = proof.cards[e.Collection.Key]
			} else {
				card = readCollectionCard(ctx, checker, u, e.Collection.Key)
			}
			return card != nil && p.Visibility.CollectionAllowed(u, card)
		}
		return p.Subject(ctx, u, string(e.Kind), e.Collection.Key.String())
	case frontend.RowEntryPlaylist:
		if e.Playlist == nil {
			return false
		}
		if proof, ok := ctx.Value(rowEntryBatchKey{}).(rowEntryBatchProof); ok && proof.viewer == u && proof.checkedPls[e.Playlist.Key] {
			return proof.playlists[e.Playlist.Key]
		}
		return p.Playlist(ctx, u, e.Playlist.Key)
	case frontend.RowEntryLiveChannel:
		return e.LiveChannel != nil && p.Subject(ctx, u, string(e.Kind), e.LiveChannel.ID)
	default:
		return false
	}
}
