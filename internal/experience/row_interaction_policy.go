package experience

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RowInteractionPolicy rechecks persisted impressions against current scope
// before recording personalization events.
type RowInteractionPolicy struct {
	Libraries           []Library
	LiveLibraries       func() []Library
	Visibility          Visibility
	Get                 func(context.Context, core.RatingKey) (*core.Title, error)
	AllowEntry          func(context.Context, *core.User, string, string) bool
	CollectionMembers   func(context.Context, *core.User, core.RatingKey, []core.NeutralEntryRef) bool
	Collections         core.Collections
	ChannelPermissions  func(context.Context, *core.User, []string) (map[string]bool, error)
	PlaylistExistsBatch func(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
	Impression          func(*core.User, core.NeutralImpression)
	EntryImpression     func(*core.User, core.NeutralImpression, core.NeutralEntryRef)
	DetailOpen          func(context.Context, *core.User, *core.Title)
}

func (p RowInteractionPolicy) libraries() []Library {
	if p.LiveLibraries != nil {
		return p.LiveLibraries()
	}
	return p.Libraries
}

func (p RowInteractionPolicy) Allowed(ctx context.Context, u *core.User, subject ReceiptSubject) bool {
	if imp := subject.Impression; imp != nil {
		if cache, ok := ctx.Value(rowInteractionBatchKey{}).(map[*core.NeutralImpression]bool); ok {
			allowed, checked := cache[imp]
			if !checked {
				allowed = p.allowedImpression(ctx, u, imp)
				cache[imp] = allowed
			}
			if !allowed {
				return false
			}
		} else if !p.allowedImpression(ctx, u, imp) {
			return false
		}
		if subject.Kind == "row" {
			return true
		}
		i := strings.LastIndex(subject.ID, "/"+subject.Kind+"/")
		if i < 0 {
			return false
		}
		id := subject.ID[i+len(subject.Kind)+2:]
		for _, entry := range imp.Entries {
			if entry.Kind == subject.Kind && entry.ID == id {
				return true
			}
		}
		return false
	}
	return false
}

// This memo exists only during one issuance policy pass, fenced by the
// shared epoch. Every subject in a page references the same trusted impression;
// checking its full membership for each card otherwise costs N squared reads.
type rowInteractionBatchKey struct{}

type rowInteractionPrefetchKey struct{}
type rowInteractionPrefetch struct {
	viewer       *core.User
	cols         map[core.RatingKey]*core.Collection
	cards        map[core.RatingKey]*core.Collection
	cardsChecked bool
	matched      map[core.RatingKey]bool
	want         map[core.RatingKey][]core.RatingKey
	checkedCols  map[core.RatingKey]bool
	channels     map[string]bool
	playlists    map[core.RatingKey]bool
	checkedPls   map[core.RatingKey]bool
}

// PrepareBatch runs at the start of one fenced IssueBatch pass to batch
// collection visibility/membership and playlist ownership checks across all
// subjects in the pass. It is never shared across retries or requests.
func (p RowInteractionPolicy) PrepareBatch(ctx context.Context, u *core.User, subjects []ReceiptSubject) context.Context {
	if u == nil || len(subjects) == 0 {
		return ctx
	}
	seenImp := map[*core.NeutralImpression]bool{}
	var colKeys, plKeys, cardKeys []core.RatingKey
	var channelIDs []string
	checkedCols := map[core.RatingKey]bool{}
	want := map[core.RatingKey][]core.RatingKey{}
	checkedPls := map[core.RatingKey]bool{}
	addCol := func(k core.RatingKey) {
		if k != 0 && !checkedCols[k] {
			checkedCols[k] = true
			colKeys = append(colKeys, k)
		}
	}
	for _, subject := range subjects {
		imp := subject.Impression
		if imp == nil || seenImp[imp] {
			continue
		}
		seenImp[imp] = true
		if imp.Collection != 0 {
			addCol(imp.Collection)
			if _, ok := want[imp.Collection]; !ok {
				if w, valid := rowViewMemberKeys(imp.Entries); valid {
					want[imp.Collection] = w
				}
			}
		}
		for _, entry := range imp.Entries {
			switch frontend.RowEntryKind(entry.Kind) {
			case frontend.RowEntryCollection:
				if k, err := core.ParseRatingKey(entry.ID); err == nil {
					cardKeys = append(cardKeys, k)
				}
			case frontend.RowEntryLiveChannel:
				channelIDs = append(channelIDs, entry.ID)
			case frontend.RowEntryPlaylist:
				if k, err := core.ParseRatingKey(entry.ID); err == nil && !checkedPls[k] {
					checkedPls[k] = true
					plKeys = append(plKeys, k)
				}
			}
		}
	}
	pf := rowInteractionPrefetch{viewer: u}
	if p.ChannelPermissions != nil && len(channelIDs) > 0 {
		pf.channels, _ = p.ChannelPermissions(ctx, u, channelIDs)
	}
	if checker, ok := p.Collections.(collectionCardHeaders); ok {
		if len(cardKeys) > 0 {
			pf.cards, _ = checker.CollectionCardHeaders(ctx, u, cardKeys)
			pf.cardsChecked = true
		}
	} else {
		for _, key := range cardKeys {
			addCol(key)
		}
	}
	if checker, ok := p.Collections.(RowViewCollectionBatchChecker); ok && len(colKeys) > 0 {
		if cols, matched, err := checker.CheckMembersBatch(ctx, u, colKeys, want, rowViewMemberScanLimit); err == nil {
			pf.cols = cols
			pf.matched = matched
			pf.want = want
			pf.checkedCols = checkedCols
		}
	}
	if p.PlaylistExistsBatch != nil && len(plKeys) > 0 {
		if pls, err := p.PlaylistExistsBatch(ctx, u.ID, plKeys); err == nil {
			pf.playlists = pls
			pf.checkedPls = checkedPls
		}
	}
	if pf.checkedCols == nil && pf.checkedPls == nil && pf.channels == nil && !pf.cardsChecked {
		return ctx
	}
	return context.WithValue(ctx, rowInteractionPrefetchKey{}, pf)
}

func (p RowInteractionPolicy) allowedImpression(ctx context.Context, u *core.User, imp *core.NeutralImpression) bool {
	if len(imp.Entries) == 0 || imp.Kind == "" {
		return false
	}
	pf, hasPF := ctx.Value(rowInteractionPrefetchKey{}).(rowInteractionPrefetch)
	if hasPF && pf.viewer != u {
		hasPF = false
	}
	if imp.Collection != 0 {
		checked := false
		if hasPF && pf.checkedCols[imp.Collection] {
			if want, valid := rowViewMemberKeys(imp.Entries); valid {
				if preWant, ok := pf.want[imp.Collection]; ok && slices.Equal(preWant, want) {
					c := pf.cols[imp.Collection]
					if c == nil || !pf.matched[imp.Collection] || !p.Visibility.CollectionAllowed(u, c) {
						return false
					}
					checked = true
				}
			}
		}
		if !checked {
			if !p.AllowEntry(ctx, u, string(frontend.RowEntryCollection), imp.Collection.String()) {
				return false
			}
			if !p.CollectionMembers(ctx, u, imp.Collection, imp.Entries) {
				return false
			}
		}
	}
	switch frontend.Placement(imp.Placement) {
	case frontend.PlacementHome:
	case frontend.PlacementLibrary:
		id, err := strconv.Atoi(imp.ViewID)
		valid := false
		for _, lib := range p.libraries() {
			if lib.ID == id {
				valid = true
				break
			}
		}
		if err != nil || !valid || !SectionAllowed(u, id) {
			return false
		}
	case frontend.PlacementDetail:
		if imp.Item == 0 {
			return false
		}
		t, err := p.Get(ctx, imp.Item)
		if err != nil || !p.Visibility.Visible(ctx, u, t) {
			return false
		}
	default:
		return false
	}
	for _, entry := range imp.Entries {
		if entry.Kind == string(frontend.RowEntryCollection) {
			if checker, ok := p.Collections.(collectionCardHeaders); ok {
				key, err := core.ParseRatingKey(entry.ID)
				if err != nil {
					return false
				}
				var card *core.Collection
				if hasPF && pf.cardsChecked {
					card = pf.cards[key]
				} else {
					card = readCollectionCard(ctx, checker, u, key)
				}
				if card == nil || !p.Visibility.CollectionAllowed(u, card) {
					return false
				}
				continue
			}
		}
		if hasPF {
			switch frontend.RowEntryKind(entry.Kind) {
			case frontend.RowEntryLiveChannel:
				if pf.channels != nil {
					if !pf.channels[entry.ID] {
						return false
					}
					continue
				}
			case frontend.RowEntryPlaylist:
				if k, err := core.ParseRatingKey(entry.ID); err == nil && pf.checkedPls[k] {
					if !pf.playlists[k] {
						return false
					}
					continue
				}
			case frontend.RowEntryCollection:
				if k, err := core.ParseRatingKey(entry.ID); err == nil && pf.checkedCols[k] {
					c := pf.cols[k]
					if c == nil || !p.Visibility.CollectionAllowed(u, c) {
						return false
					}
					continue
				}
			}
		}
		if !p.AllowEntry(ctx, u, entry.Kind, entry.ID) {
			return false
		}
	}
	return true
}

// AllowedAction distinguishes a row exposure from one actually visible entry.
func (p RowInteractionPolicy) AllowedAction(subject ReceiptSubject, kind frontend.RowInteractionKind) bool {
	if subject.Impression == nil {
		return false
	}
	if subject.Kind == "row" {
		return kind == frontend.RowPlay || (kind == frontend.RowImpression && p.Impression != nil)
	}
	if _, ok := receiptEntry(subject); !ok {
		return false
	}
	switch kind {
	case frontend.RowImpression:
		return p.EntryImpression != nil
	case frontend.RowClick, frontend.RowPlay:
		return true
	}
	return false
}

func receiptEntry(subject ReceiptSubject) (core.NeutralEntryRef, bool) {
	if subject.Impression != nil {
		for _, entry := range subject.Impression.Entries {
			if subject.Kind == entry.Kind && subject.ID == subject.Impression.RowID+"/"+entry.Kind+"/"+entry.ID {
				return entry, true
			}
		}
	}
	return core.NeutralEntryRef{}, false
}

func (p RowInteractionPolicy) Record(ctx context.Context, u *core.User, subject ReceiptSubject, kind frontend.RowInteractionKind) error {
	// The playback watch event already supplies the play reward. A row play
	// receipt acknowledges its validated attribution without counting another
	// impression (which would inflate the number of times this shelf was seen).
	if kind == frontend.RowPlay && subject.Impression != nil && (subject.Kind == "row" || p.AllowedAction(subject, kind)) {
		return nil
	}
	if kind == frontend.RowImpression && subject.Impression != nil {
		if subject.Kind == "row" && p.Impression != nil {
			p.Impression(u, *subject.Impression)
			return nil
		}
		if entry, ok := receiptEntry(subject); ok && p.EntryImpression != nil {
			p.EntryImpression(u, *subject.Impression, entry)
			return nil
		}
		return ErrReceipt
	}
	if kind == frontend.RowClick && subject.Kind == string(frontend.RowEntryTitle) {
		idx := strings.LastIndex(subject.ID, "/title/")
		if idx < 0 {
			return ErrReceipt
		}
		key, err := core.ParseRatingKey(subject.ID[idx+len("/title/"):])
		if err != nil {
			return err
		}
		title, err := p.Get(ctx, key)
		if err != nil || !p.Visibility.Visible(ctx, u, title) {
			return ErrReceipt
		}
		p.DetailOpen(ctx, u, title)
		return nil
	}
	// Non-title destinations have no title-detail reward. Their validated
	// selection still remains deduplicated in the receipt action ledger.
	if kind == frontend.RowClick && subject.Kind != "row" {
		if _, ok := receiptEntry(subject); ok {
			return nil
		}
	}
	return errors.New("experience: unsupported row interaction")
}
