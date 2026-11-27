// Personalized home rows are extra CollectionFolders on this engine.
// Jellyfin Web and jellyfin-androidtv turn each user view into a
// "Recently Added in {name}" / "Latest in {name}" shelf from /Items/Latest,
// in view order, with no client sort. Swiftfin, Findroid, Streamyfin and
// JellyCon open a view with SortBy=SortName; these folders still return rank
// order. Infuse on Jellyfin calls /Items/Latest per view after /UserViews.
// The Emby SDK sample browses /Items?ParentId. Continue Watching and Up Next
// stay on their own routes. See the owner Apps pages for the on/count/kind
// controls. Plex does not use this path.
package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

const (
	personalRowFetch    = 50
	personalRowMax      = 12
	personalRowDefault  = 4
	playCreditCap       = 4096
	personalIdentityTTL = 30 * time.Second
)

// personalHandleDelay waits out a Swiftfin home burst (Latest per view, then
// Resume and Next Up) before minting row receipts. Issuing them on the shared
// shelf load writes one receipt per title and held the first Latest for seconds.
var personalHandleDelay = 750 * time.Millisecond

// personalHomeConfig is the owner's "Home rows shown" count for Jellyfin and
// Emby. Which rows exist, and in what order, is the Home Recommendations page;
// this only limits how many of them the app lists. Missing means four.
type personalHomeConfig struct {
	count int
}

func personalHomeFrom(settings frontend.Settings) personalHomeConfig {
	cfg := personalHomeConfig{count: personalRowDefault}
	if value, ok := settings["homeRowCount"].(float64); ok && value >= 0 && value <= personalRowMax && value == math.Trunc(value) {
		cfg.count = int(value)
	}
	return cfg
}

func (i *instance) personalConfig() personalHomeConfig {
	if i.host.Config == nil {
		return personalHomeFrom(nil)
	}
	return personalHomeFrom(i.host.Config())
}

func validatePersonalSettings(s frontend.Settings) error {
	for key, value := range s {
		switch key {
		case "port", "serverName", "discovery", "quickConnect", "publicViewerPicker":
		case "homeRowCount":
			n, ok := value.(float64)
			if !ok || n < 0 || n > personalRowMax || n != math.Trunc(n) {
				return fmt.Errorf("invalid %s", key)
			}
		default:
			return fmt.Errorf("unknown setting %s", key)
		}
	}
	return nil
}

// personalViewNumber is stable for one viewer and one row identity.
func personalViewNumber(viewerID int64, rowID string) int64 {
	sum := sha256.Sum256([]byte(strconv.FormatInt(viewerID, 10) + "\n" + rowID))
	n := int64(binary.BigEndian.Uint64(sum[:8]) & 0x3FFFFFFFFFFFFFFF)
	if n == 0 {
		n = 1
	}
	return n | ids.PersonalViewBit
}

type personalShelf struct {
	number    int64
	wireID    string
	name      string
	kind      string
	count     int
	placement frontend.Placement
	viewID    string
	row       frontend.Row
	titles    []frontend.Title
}

type personalIdentitySlot struct {
	at      time.Time
	gen     int64
	shelves []personalShelf
}

type personalLoad struct {
	done    chan struct{}
	shelves []personalShelf
}

type personalCredit struct {
	viewer    frontend.Viewer
	device    frontend.Device
	placement frontend.Placement
	viewID    string
	rowID     string
	row       frontend.Row
}

type personalIdentityCache struct {
	mu         sync.Mutex
	items      map[string]personalIdentitySlot
	saved      map[string][]personalShelf
	loaded     map[string]personalIdentitySlot
	loading    map[string]*personalLoad
	inflight   map[string]bool
	pending    []personalCredit
	issueTimer *time.Timer
}

// rowSnapshotter is the authorized home without minting receipts. Silo uses
// the same split so a shelf read is not an impression.
type rowSnapshotter interface {
	SnapshotRows(context.Context, frontend.RowRequest) ([]frontend.Row, error)
}

const personalWarmDevice = "ether-personal-home"

type policyGeneration interface {
	PolicyGeneration(context.Context) (int64, error)
}

func (i *instance) policyGeneration(ctx context.Context) int64 {
	src, ok := i.host.Experience.(policyGeneration)
	if !ok || src == nil {
		return 0
	}
	gen, err := src.PolicyGeneration(ctx)
	if err != nil || gen < 0 {
		return 0
	}
	return gen
}

func personalCacheKey(protocol string, v *frontend.Viewer, dev frontend.Device, cfg personalHomeConfig) string {
	return fmt.Sprintf("%s\n%d\n%s\n%s\n%s", protocol, v.ID, dev.ID, dev.Language, personalConfigKey(cfg))
}

// personalShelves is the cached identity list. It never builds row items and
// never waits on a model. A request serves the last list for this device, or
// the last list saved for this viewer, and refreshes in the background.
func (i *instance) personalShelves(r *http.Request, v *frontend.Viewer) []personalShelf {
	cfg := i.personalConfig()
	if cfg.count == 0 || v == nil || i.host.Experience == nil {
		return nil
	}
	dev := device(r)
	if dev.ID == "" {
		return nil
	}
	key := personalCacheKey(string(i.protocol), v, dev, cfg)
	gen := i.policyGeneration(r.Context())
	i.personal.mu.Lock()
	slot, ok := i.personal.items[key]
	saved := i.personal.saved[personalSavedKey(string(i.protocol), v.ID, cfg)]
	i.personal.mu.Unlock()
	fresh := ok && time.Since(slot.at) < personalIdentityTTL && slot.gen == gen
	if !fresh {
		i.warmPersonalIdentities(key, gen, v, dev)
	}
	if ok && len(slot.shelves) > 0 {
		return cloneShelves(slot.shelves)
	}
	if len(saved) > 0 {
		return cloneShelves(saved)
	}
	return nil
}

func (i *instance) warmPersonalIdentities(key string, gen int64, v *frontend.Viewer, dev frontend.Device) {
	viewer := *v
	i.personal.mu.Lock()
	if i.personal.inflight == nil {
		i.personal.inflight = map[string]bool{}
	}
	if i.personal.inflight[key] {
		i.personal.mu.Unlock()
		return
	}
	i.personal.inflight[key] = true
	i.personal.mu.Unlock()
	go func() {
		defer func() {
			i.personal.mu.Lock()
			delete(i.personal.inflight, key)
			i.personal.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(i.life, 15*time.Second)
		defer cancel()
		loaded := i.loadPersonalShelves(ctx, &viewer, dev)
		if loaded == nil {
			return
		}
		identities := make([]personalShelf, len(loaded))
		for n, shelf := range loaded {
			shelf.titles = nil
			shelf.row.Entries = nil
			shelf.row.Items = nil
			shelf.row.Handle = ""
			identities[n] = shelf
		}
		if len(identities) == 0 {
			return
		}
		i.storePersonalIdentities(key, gen, &viewer, identities)
	}()
}

func (i *instance) expirePersonalShelves(viewerID int64) {
	prefix := fmt.Sprintf("%s\n%d\n", i.protocol, viewerID)
	i.personal.mu.Lock()
	defer i.personal.mu.Unlock()
	for key, slot := range i.personal.items {
		if strings.HasPrefix(key, prefix) {
			slot.at = time.Time{}
			i.personal.items[key] = slot
		}
	}
	for key := range i.personal.loaded {
		if strings.HasPrefix(key, prefix) {
			delete(i.personal.loaded, key)
		}
	}
}

func cloneShelves(in []personalShelf) []personalShelf {
	out := make([]personalShelf, len(in))
	copy(out, in)
	return out
}

func cloneLoaded(in []personalShelf) []personalShelf {
	if in == nil {
		return nil
	}
	out := make([]personalShelf, len(in))
	for i, shelf := range in {
		shelf.titles = append([]frontend.Title(nil), shelf.titles...)
		out[i] = shelf
	}
	return out
}

func personalSavedKey(protocol string, viewerID int64, cfg personalHomeConfig) string {
	return fmt.Sprintf("%s\n%d\n%s", protocol, viewerID, personalConfigKey(cfg))
}

func personalConfigKey(cfg personalHomeConfig) string {
	return "rows\n" + strconv.Itoa(cfg.count)
}

type storedPersonalShelf struct {
	RankedTop10    bool   `json:"rankedTop10,omitempty"`
	PreviewOnFocus bool   `json:"previewOnFocus,omitempty"`
	Number         int64  `json:"number"`
	WireID         string `json:"wireID"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Count          int    `json:"count"`
}

func encodeShelves(in []personalShelf) ([]byte, error) {
	out := make([]storedPersonalShelf, len(in))
	for i, shelf := range in {
		out[i] = storedPersonalShelf{PreviewOnFocus: shelf.row.PreviewOnFocus, RankedTop10: shelf.row.RankedTop10, Number: shelf.number, WireID: shelf.wireID, Name: shelf.name, Kind: shelf.kind, Count: shelf.count}
	}
	return json.Marshal(out)
}

func decodeShelves(body []byte) ([]personalShelf, error) {
	var stored []storedPersonalShelf
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, err
	}
	out := make([]personalShelf, 0, len(stored))
	for _, shelf := range stored {
		if shelf.WireID == "" || shelf.Name == "" {
			continue
		}
		out = append(out, personalShelf{number: shelf.Number, wireID: shelf.WireID, name: shelf.Name, kind: shelf.Kind, count: shelf.Count, placement: frontend.PlacementHome, row: frontend.Row{PreviewOnFocus: shelf.PreviewOnFocus, RankedTop10: shelf.RankedTop10}})
	}
	return out, nil
}

func (i *instance) storePersonalIdentities(key string, gen int64, v *frontend.Viewer, identities []personalShelf) {
	cfg := i.personalConfig()
	i.personal.mu.Lock()
	if i.personal.items == nil {
		i.personal.items = map[string]personalIdentitySlot{}
	}
	if i.personal.saved == nil {
		i.personal.saved = map[string][]personalShelf{}
	}
	i.personal.items[key] = personalIdentitySlot{at: time.Now(), gen: gen, shelves: identities}
	savedKey := personalSavedKey(string(i.protocol), v.ID, cfg)
	i.personal.saved[savedKey] = cloneShelves(identities)
	prefix := fmt.Sprintf("%s\n%d\n", i.protocol, v.ID)
	for existing := range i.personal.items {
		if existing != key && strings.HasPrefix(existing, prefix) {
			delete(i.personal.items, existing)
		}
	}
	i.personal.mu.Unlock()
	if i.host.PersonalHomes == nil {
		return
	}
	body, err := encodeShelves(identities)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(i.life, 2*time.Second)
	defer cancel()
	if err := i.host.PersonalHomes.SavePersonalHome(ctx, string(i.protocol), v.ID, personalConfigKey(cfg), body); err != nil && i.host.Log != nil {
		i.host.Log.Warn("personal home save", "err", err)
	}
}

// PreparePersonalHomes loads the last saved library list before the listener
// accepts clients, then refreshes each viewer without waiting for a model.
func (i *instance) PreparePersonalHomes(ctx context.Context) {
	if i.host.PersonalHomes == nil {
		return
	}
	cfg := i.personalConfig()
	if cfg.count == 0 {
		return
	}
	users, err := i.host.PersonalHomes.PersonalHomeViewers(ctx)
	if err != nil {
		return
	}
	for _, user := range users {
		body, err := i.host.PersonalHomes.LoadPersonalHome(ctx, string(i.protocol), user.ID, personalConfigKey(cfg))
		if err != nil || len(body) == 0 {
			continue
		}
		shelves, err := decodeShelves(body)
		if err != nil || len(shelves) == 0 {
			continue
		}
		i.personal.mu.Lock()
		if i.personal.saved == nil {
			i.personal.saved = map[string][]personalShelf{}
		}
		i.personal.saved[personalSavedKey(string(i.protocol), user.ID, cfg)] = shelves
		i.personal.mu.Unlock()
	}
	dev := frontend.Device{ID: personalWarmDevice, Client: "Ether"}
	gen := i.policyGeneration(ctx)
	for _, user := range users {
		viewer := user
		key := personalCacheKey(string(i.protocol), &viewer, dev, cfg)
		i.warmPersonalIdentities(key, gen, &viewer, dev)
	}
}

// RefreshPersonalHome rebuilds one viewer's libraries after a model lands.
// It does not wait for the build that just finished.
func (i *instance) RefreshPersonalHome(userID int64) {
	if i.host.PersonalHomes == nil || userID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(i.life, 2*time.Second)
	defer cancel()
	users, err := i.host.PersonalHomes.PersonalHomeViewers(ctx)
	if err != nil {
		return
	}
	cfg := i.personalConfig()
	dev := frontend.Device{ID: personalWarmDevice, Client: "Ether"}
	for _, user := range users {
		if user.ID != userID {
			continue
		}
		viewer := user
		i.warmPersonalIdentities(personalCacheKey(string(i.protocol), &viewer, dev, cfg), i.policyGeneration(ctx), &viewer, dev)
		return
	}
}

func (i *instance) loadPersonalShelves(ctx context.Context, v *frontend.Viewer, dev frontend.Device) []personalShelf {
	cfg := i.personalConfig()
	if cfg.count == 0 || v == nil || i.host.Experience == nil || dev.ID == "" {
		return nil
	}
	rows, err := i.personalHomeRows(ctx, v, dev)
	if err != nil {
		return nil
	}
	return shelvesFromRows(cfg, v, rows, frontend.PlacementHome, "")
}

func (i *instance) loadPersonalLibraryShelves(ctx context.Context, v *frontend.Viewer, dev frontend.Device, viewID string, budget int) ([]personalShelf, error) {
	if v == nil || i.host.Experience == nil || viewID == "" {
		return nil, nil
	}
	cfg := i.personalConfig()
	if budget > cfg.count {
		cfg.count = budget
	}
	rows, err := i.personalLibraryRows(ctx, v, dev, viewID)
	if err != nil {
		return nil, err
	}
	return shelvesFromRows(cfg, v, rows, frontend.PlacementLibrary, viewID), nil
}

func shelvesFromRows(cfg personalHomeConfig, v *frontend.Viewer, rows []frontend.Row, placement frontend.Placement, viewID string) []personalShelf {
	if len(rows) == 0 {
		return []personalShelf{}
	}
	alwaysCount := 0
	seenPre := map[string]bool{}
	for _, row := range rows {
		if row.ID == "" || seenPre[row.ID] || nowAvailableExperienceRow(row) || !personalShelfCompatible(row) || !row.Layout.Always {
			continue
		}
		if len(personalTitles(row)) == 0 && personalCollectionCount(row) == 0 {
			continue
		}
		seenPre[row.ID] = true
		alwaysCount++
	}
	optionalBudget := max(0, cfg.count-alwaysCount)
	optionalUsed := 0
	out := make([]personalShelf, 0, max(cfg.count, alwaysCount))
	seen := map[string]bool{}
	var pinned []personalShelf
	for _, row := range rows {
		if row.ID == "" || seen[row.ID] {
			continue
		}
		available := nowAvailableExperienceRow(row)
		always := !available && personalShelfCompatible(row) && row.Layout.Always
		if !available && !always && (!personalShelfCompatible(row) || optionalUsed >= optionalBudget) {
			continue
		}
		titles := personalTitles(row)
		collectionCount := personalCollectionCount(row)
		if len(titles) == 0 && collectionCount == 0 {
			continue
		}
		seen[row.ID] = true
		rowKey := row.ID
		if viewID != "" {
			rowKey = "library:" + viewID + ":" + row.ID
		}
		number := personalViewNumber(v.ID, rowKey)
		wire, err := ids.GUID(ids.View, number)
		if err != nil {
			continue
		}
		shelf := personalShelf{number: number, wireID: wire, name: personalLibraryName(row), kind: personalCollectionType(titles), count: len(titles), placement: placement, viewID: viewID, row: row, titles: titles}
		if collectionCount > 0 {
			shelf.count = collectionCount
			if row.Layout.Present.Card == "landscape" {
				shelf.kind = "mixed"
			}
		}
		if available {
			pinned = append(pinned, shelf)
			continue
		}
		if !always {
			optionalUsed++
		}
		out = append(out, shelf)
	}
	return append(out, pinned...)
}

// The available half of a split watchlist does not consume the personalized-row count.
func nowAvailableExperienceRow(row frontend.Row) bool {
	return frontend.WatchlistHubAvailable(row.ID)
}

// personalShelfCompatible reports whether a Recommendations row becomes a
// personal Home folder. Continue, Next Up and Jump back in stay the client's
// own Resume and Next Up sections, Live stays Live TV, and plugin surface rows
// have no titles to list.
func personalShelfCompatible(row frontend.Row) bool {
	switch row.Purpose {
	case frontend.RowContinue, frontend.RowNextUp, frontend.RowLive, frontend.RowRecentContexts:
		return false
	}
	return row.Layout.Present.Surface == nil
}

func personalCollectionCount(row frontend.Row) int {
	count := 0
	for _, entry := range row.Entries {
		if entry.Kind == frontend.RowEntryCollection && entry.Collection != nil {
			count++
		}
	}
	return count
}

func personalTitles(row frontend.Row) []frontend.Title {
	seen := map[frontend.RatingKey]bool{}
	var out []frontend.Title
	add := func(title frontend.Title) {
		if title.Key == 0 || seen[title.Key] {
			return
		}
		seen[title.Key] = true
		out = append(out, title)
	}
	if len(row.Entries) > 0 {
		for _, entry := range row.Entries {
			if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
				add(*entry.Title)
			}
		}
		return out
	}
	for _, title := range row.Items {
		add(title)
	}
	return out
}

func personalLibraryName(row frontend.Row) string {
	title := strings.TrimSpace(row.Title)
	if row.Purpose == frontend.RowWatchlist && title != "" {
		return title
	}
	if seed, ok := personalSeed(title, "because you watched ", "because you've watched "); ok {
		return seed + " and Similar"
	}
	if seed, ok := personalSeed(title, "because you liked ", "because you like "); ok {
		return seed + " and Similar"
	}
	if title != "" {
		return title
	}
	switch row.Kind {
	case "top-10", "top-10-today":
		return "For You"
	case "trending", "trending-in-your-taste", "trending-with-fans":
		return "Trending"
	case "because-you-watched":
		return "Similar Picks"
	case "fans-also-loved":
		return "Fans Also Loved"
	case "new-releases", "new-this-week":
		return "New Releases"
	default:
		return "For You"
	}
}

func personalSeed(title string, prefixes ...string) (string, bool) {
	lower := strings.ToLower(title)
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			seed := strings.Trim(strings.TrimSpace(title[len(prefix):]), " .…")
			if seed != "" {
				return seed, true
			}
		}
	}
	return "", false
}

func personalChildCount(shelf personalShelf) int {
	if len(shelf.titles) > 0 {
		return len(shelf.titles)
	}
	return shelf.count
}

func personalCollectionType(titles []frontend.Title) string {
	shows := 0
	for _, title := range titles {
		if title.Type == "show" || title.Type == "season" || title.Type == "episode" {
			shows++
		}
	}
	if len(titles) > 0 && shows == len(titles) {
		return "tvshows"
	}
	return "movies"
}

func (i *instance) personalByNumber(r *http.Request, v *frontend.Viewer, number int64) (personalShelf, bool) {
	for _, shelf := range i.loadedPersonalShelves(r.Context(), v, device(r)) {
		if shelf.number == number {
			return shelf, true
		}
	}
	return personalShelf{}, false
}

// loadedPersonalShelves reads home once for a burst of Latest calls. Swiftfin
// asks for every view at once; each call used to rebuild and authorize home.
func (i *instance) loadedPersonalShelves(ctx context.Context, v *frontend.Viewer, dev frontend.Device) []personalShelf {
	cfg := i.personalConfig()
	if cfg.count == 0 || v == nil || i.host.Experience == nil || dev.ID == "" {
		return nil
	}
	key := personalCacheKey(string(i.protocol), v, dev, cfg)
	gen := i.policyGeneration(ctx)
	i.personal.mu.Lock()
	if i.personal.loaded != nil {
		if slot, ok := i.personal.loaded[key]; ok && slot.gen == gen && time.Since(slot.at) < personalIdentityTTL {
			shelves := cloneLoaded(slot.shelves)
			i.personal.mu.Unlock()
			return shelves
		}
	}
	if i.personal.loading != nil {
		if call, ok := i.personal.loading[key]; ok {
			i.personal.mu.Unlock()
			select {
			case <-call.done:
				return cloneLoaded(call.shelves)
			case <-ctx.Done():
				return nil
			}
		}
	}
	call := &personalLoad{done: make(chan struct{})}
	if i.personal.loading == nil {
		i.personal.loading = map[string]*personalLoad{}
	}
	i.personal.loading[key] = call
	i.personal.mu.Unlock()

	loaded := i.loadPersonalShelves(ctx, v, dev)
	if loaded != nil {
		i.rememberLoaded(key, gen, v, loaded)
	}
	call.shelves = loaded
	close(call.done)
	i.personal.mu.Lock()
	delete(i.personal.loading, key)
	i.personal.mu.Unlock()
	return cloneLoaded(loaded)
}

func (i *instance) rememberLoaded(key string, gen int64, v *frontend.Viewer, loaded []personalShelf) {
	i.personal.mu.Lock()
	if i.personal.loaded == nil {
		i.personal.loaded = map[string]personalIdentitySlot{}
	}
	i.personal.loaded[key] = personalIdentitySlot{at: time.Now(), gen: gen, shelves: cloneLoaded(loaded)}
	i.personal.mu.Unlock()
	if len(loaded) == 0 {
		return
	}
	identities := make([]personalShelf, len(loaded))
	for n, shelf := range loaded {
		shelf.titles = nil
		shelf.row.Entries = nil
		shelf.row.Items = nil
		shelf.row.Handle = ""
		identities[n] = shelf
	}
	i.storePersonalIdentities(key, gen, v, identities)
}

func (i *instance) personalFolder(r *http.Request, v *frontend.Viewer, shelf personalShelf) map[string]any {
	item := map[string]any{
		"Id": shelf.wireID, "Name": shelf.name, "Type": "CollectionFolder", "CollectionType": shelf.kind,
		"IsFolder": true, "ServerId": i.info.ID, "SortName": shelf.name, "DisplayPreferencesId": shelf.wireID,
		"CanDelete": false, "CanDownload": false, "ProviderIds": map[string]string{}, "ChildCount": personalChildCount(shelf),
	}
	if i.protocol == "emby" {
		i.embyViewDTO(item)
	}
	if i.protocol == "jellyfin" {
		i.jellyfinCollectionFolder(item, shelf.wireID)
		item["ParentId"] = jellyfinRootID()
		item["ChildCount"] = personalChildCount(shelf)
	}
	return item
}

func (i *instance) prependPersonalViews(r *http.Request, v *frontend.Viewer, items []any) []any {
	shelves := i.personalShelves(r, v)
	if len(shelves) == 0 {
		return items
	}
	head := make([]any, 0, len(shelves)+len(items))
	for _, shelf := range shelves {
		head = append(head, i.personalFolder(r, v, shelf))
	}
	return append(head, items...)
}

func (i *instance) writePersonalFolder(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, number int64) {
	for _, shelf := range i.personalShelves(r, v) {
		if shelf.number == number {
			i.replyItems(w, r, i.personalFolder(r, v, shelf))
			return
		}
	}
	http.NotFound(w, r)
}

func (i *instance) personalTitlePage(r *http.Request, v *frontend.Viewer, shelf personalShelf, q frontend.ItemQuery, latest bool) ([]frontend.Title, int, error) {
	titles := append([]frontend.Title(nil), shelf.titles...)
	if len(q.Types) > 0 || q.Watched != nil || q.Favorite != nil || q.FilterByProviderIDs || latest {
		keys := make([]frontend.RatingKey, 0, len(titles))
		for _, title := range titles {
			keys = append(keys, title.Key)
		}
		states, err := i.host.Experience.UserData(r.Context(), v, keys)
		if err != nil {
			return nil, 0, err
		}
		played, filterPlayed := frontend.LatestPlayedFilter(r.Context())
		kept := titles[:0]
		for _, title := range titles {
			if q.FilterByProviderIDs && !titleMatchesProviderIDs(title, q.ProviderIDs) {
				continue
			}
			state := states[title.Key]
			watched := state.ViewCount > 0
			if latest && filterPlayed && watched != played {
				continue
			}
			if q.Watched != nil && watched != *q.Watched {
				continue
			}
			if q.Favorite != nil && state.IsFavorite != *q.Favorite {
				continue
			}
			if len(q.Types) > 0 && !titleTypeMatches(title.Type, q.Types) {
				continue
			}
			kept = append(kept, title)
		}
		titles = kept
	}
	if !latest && !shelf.row.RankedTop10 {
		if sortByRandom(r) {
			rand.Shuffle(len(titles), func(a, b int) { titles[a], titles[b] = titles[b], titles[a] })
		} else if personalSortAsked(r, q) {
			sortPersonalTitles(titles, q)
		}
	}
	if shelf.row.RankedTop10 {
		titles = titles[:min(10, len(titles))]
	}
	total := len(titles)
	offset, limit := q.Offset, q.Limit
	if limit <= 0 {
		limit = total
	}
	return paging.Slice(titles, offset, limit), total, nil
}

// personalSortAsked reports whether the app picked a sort for a personal row
// opened as a folder. Apps open every view folder with SortBy=SortName
// ascending, so that default keeps the row's own (curated) order; any other
// choice from the app's sort menu reorders the row.
func personalSortAsked(r *http.Request, q frontend.ItemQuery) bool {
	if r == nil || q.Sort == "" || len(route.Query(r.URL.Query(), "SortBy")) == 0 {
		return false
	}
	return !(q.Sort == frontend.SortTitle && !q.Desc)
}

// sortPersonalTitles orders a row's titles in place by the facts the titles
// already carry. Orders that need viewer state keep the row's order.
func sortPersonalTitles(titles []frontend.Title, q frontend.ItemQuery) {
	var less func(a, b *frontend.Title) int
	cmpFloat := func(x, y float64) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	switch q.Sort {
	case frontend.SortTitle:
		less = func(a, b *frontend.Title) int {
			return strings.Compare(frontend.EffectiveSortTitle(a.Title, a.SortTitle), frontend.EffectiveSortTitle(b.Title, b.SortTitle))
		}
	case frontend.SortRating:
		less = func(a, b *frontend.Title) int { return cmpFloat(a.Rating, b.Rating) }
	case frontend.SortPopularity:
		less = func(a, b *frontend.Title) int { return cmpFloat(a.Popularity, b.Popularity) }
	case frontend.SortDuration:
		less = func(a, b *frontend.Title) int { return cmpFloat(float64(a.Runtime), float64(b.Runtime)) }
	case frontend.SortReleased, frontend.SortAdded, frontend.SortYear:
		less = func(a, b *frontend.Title) int {
			if c := a.Released.Compare(b.Released); c != 0 {
				return c
			}
			return cmpFloat(float64(a.Year), float64(b.Year))
		}
	default:
		return
	}
	sort.SliceStable(titles, func(x, y int) bool {
		c := less(&titles[x], &titles[y])
		if q.Desc {
			return c > 0
		}
		return c < 0
	})
}

func titleTypeMatches(typ frontend.MediaType, types []frontend.MediaType) bool {
	for _, want := range types {
		if typ == want {
			return true
		}
	}
	return false
}

func (i *instance) personalHomeRows(ctx context.Context, v *frontend.Viewer, dev frontend.Device) ([]frontend.Row, error) {
	req := frontend.RowRequest{Viewer: v, Device: dev, Placement: frontend.PlacementHome, Limit: personalRowFetch}
	if snap, ok := i.host.Experience.(rowSnapshotter); ok {
		return snap.SnapshotRows(ctx, req)
	}
	return i.host.Experience.Rows(ctx, req)
}

func (i *instance) personalLibraryRows(ctx context.Context, v *frontend.Viewer, dev frontend.Device, viewID string) ([]frontend.Row, error) {
	req := frontend.RowRequest{Viewer: v, Device: dev, Placement: frontend.PlacementLibrary, ViewID: viewID, Limit: personalRowFetch}
	if snap, ok := i.host.Experience.(rowSnapshotter); ok {
		return snap.SnapshotRows(ctx, req)
	}
	return i.host.Experience.Rows(ctx, req)
}

// parseLibraryParentView extracts a library ViewID from a ParentId query
// parameter when present. scoped is false when ParentId is absent or names the
// root folder; valid is false when duplicate ParentId parameters are supplied.
func parseLibraryParentView(r *http.Request) (viewID string, scoped bool, valid bool) {
	values := route.Query(r.URL.Query(), "ParentId")
	if len(values) == 0 {
		return "", false, true
	}
	if len(values) > 1 {
		return "", true, false
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" || strings.EqualFold(raw, jellyfinRootID()) {
		return "", false, true
	}
	n, err := ids.ParseGUID(ids.View, raw)
	if err != nil || n <= 0 || ids.PersonalView(n) || n == collectionsViewNumber || n == liveTVViewNumber {
		return "", true, true
	}
	return ids.Decimal(n), true, true
}

func (i *instance) notePersonalDelivery(r *http.Request, v *frontend.Viewer, shelf personalShelf, titles []frontend.Title) {
	if len(titles) == 0 {
		return
	}
	dev := device(r)
	if shelf.row.Handle == "" {
		i.queuePersonalCredit(v, dev, shelf.placement, shelf.viewID, shelf.row, titles)
		return
	}
	i.reportPersonalImpression(r.Context(), v, dev, shelf.row.Handle)
	for _, title := range titles {
		i.credits.remember(v.ID, dev.ID, ids.RatingKeyGUID(title.Key), shelf.row.Handle)
	}
}

func (i *instance) reportPersonalImpression(ctx context.Context, v *frontend.Viewer, dev frontend.Device, handle string) {
	reporter, ok := i.host.Experience.(frontend.RowInteractionReporter)
	if !ok || handle == "" {
		return
	}
	err := reporter.ReportRowInteraction(ctx, frontend.RowInteraction{Viewer: v, Device: dev, Protocol: i.protocol, Handle: handle, Kind: frontend.RowImpression})
	if err != nil && i.host.Log != nil {
		i.host.Log.Warn("row impression failed", "error", err)
	}
}

func (i *instance) queuePersonalCredit(v *frontend.Viewer, dev frontend.Device, placement frontend.Placement, viewID string, row frontend.Row, titles []frontend.Title) {
	rowID := row.ID
	if v == nil || dev.ID == "" || rowID == "" || len(titles) == 0 || i.host.Experience == nil {
		return
	}
	if placement == "" {
		placement = frontend.PlacementHome
	}
	// Keep only the title entries actually sent in this response.
	row.Entries = make([]frontend.RowEntry, 0, min(len(titles), personalRowFetch))
	for n := range titles[:min(len(titles), personalRowFetch)] {
		title := titles[n]
		row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &title})
	}
	credit := personalCredit{viewer: *v, device: dev, placement: placement, viewID: viewID, rowID: rowID, row: row}
	i.personal.mu.Lock()
	// Repeated Latest requests for the same shelf share one pending receipt.
	for n, pending := range i.personal.pending {
		if pending.viewer.ID == v.ID && pending.device.ID == dev.ID && pending.placement == placement && pending.viewID == viewID && pending.rowID == rowID {
			i.personal.pending[n] = credit
			i.personal.mu.Unlock()
			return
		}
	}
	if len(i.personal.pending) >= 128 {
		i.personal.pending = i.personal.pending[1:]
	}
	i.personal.pending = append(i.personal.pending, credit)
	if i.personal.issueTimer != nil {
		i.personal.issueTimer.Stop()
	}
	i.personal.issueTimer = time.AfterFunc(personalHandleDelay, func() { i.flushPersonalCredits() })
	i.personal.mu.Unlock()
}

func (i *instance) flushPersonalCredits() {
	i.personal.mu.Lock()
	pending := i.personal.pending
	i.personal.pending = nil
	i.personal.mu.Unlock()
	if len(pending) == 0 || i.life.Err() != nil {
		return
	}
	groups := map[string][]personalCredit{}
	for _, credit := range pending {
		key := personalCacheKey(string(i.protocol), &credit.viewer, credit.device, i.personalConfig()) + "\n" + string(credit.placement) + "\n" + credit.viewID
		groups[key] = append(groups[key], credit)
	}
	for _, credits := range groups {
		viewer := credits[0].viewer
		placement := credits[0].placement
		if placement == "" {
			placement = frontend.PlacementHome
		}
		ctx, cancel := context.WithTimeout(i.life, 15*time.Second)
		req := frontend.RowRequest{Viewer: &viewer, Device: credits[0].device, Placement: placement, ViewID: credits[0].viewID, Limit: personalRowFetch}
		var rows []frontend.Row
		var err error
		if issuer, ok := i.host.Experience.(frontend.RowReceiptIssuer); ok {
			// Coalesce the burst by row before issuing a small batch. No Home
			// rebuild, no receipts for undelivered entries.
			seen := map[string]bool{}
			for _, credit := range credits {
				if !seen[credit.rowID] && len(rows) < personalRowMax {
					seen[credit.rowID] = true
					rows = append(rows, credit.row)
				}
			}
			rows, err = issuer.IssueRowReceipts(ctx, req, rows)
		} else {
			rows, err = i.host.Experience.Rows(ctx, req)
		}
		cancel()
		if err != nil {
			if i.host.Log != nil {
				i.host.Log.Warn("personal home receipts", "err", err)
			}
			continue
		}
		for _, row := range rows {
			if row.Handle == "" {
				continue
			}
			i.reportPersonalImpression(i.life, &viewer, credits[0].device, row.Handle)
			for _, entry := range row.Entries {
				if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
					i.credits.remember(viewer.ID, credits[0].device.ID, ids.RatingKeyGUID(entry.Title.Key), row.Handle)
				}
			}
		}
	}
}

func (i *instance) writePersonalItems(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, number int64) {
	shelf, ok := i.personalByNumber(r, v, number)
	if !ok {
		// A personalised view id read from an earlier /UserViews can rotate out
		// before a client opens it (Swiftfin, Findroid and Streamyfin browse a
		// view folder with SortBy=SortName). Real Jellyfin answers an unknown
		// parent with an empty page, never 404, and the Latest/Resume paths
		// below already do this; do the same here so the folder still opens.
		offset := queryInt(r, "StartIndex", 0)
		i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
		return
	}
	q := itemQuery(r)
	if err := navigationQuery(r, &q, false); err != nil {
		http.Error(w, "Invalid navigation anchor", http.StatusBadRequest)
		return
	}
	if personalCollectionCount(shelf.row) > 0 {
		i.writePersonalCollections(w, r, shelf, q, false)
		return
	}
	page, total, err := i.personalTitlePage(r, v, shelf, q, false)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	i.notePersonalDelivery(r, v, shelf, page)
	i.page(w, withRankedRow(withPreviewRowArtwork(r, shelf.row.PreviewOnFocus), shelf.row.RankedTop10, page, q.Offset), v, frontend.Page{Items: page, Total: total}, q.Offset)
}

func (i *instance) writePersonalActivity(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, number int64, latest bool, offset, limit int) {
	shelf, ok := i.personalByNumber(r, v, number)
	if !ok {
		// Personal shelves follow this viewer's rows, so a view id from an
		// earlier /UserViews can be gone by the time a client asks for its
		// latest items. Home screens (Swiftfin) block on a failed shelf; an
		// empty one lets them render.
		if latest {
			i.replyItems(w, r, []any{})
		} else {
			i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
		}
		return
	}
	q := frontend.ItemQuery{Offset: offset, Limit: limit}
	if !latest {
		for _, value := range route.Query(r.URL.Query(), "MediaTypes") {
			for _, typ := range strings.Split(value, ",") {
				if !strings.EqualFold(strings.TrimSpace(typ), "Video") {
					http.Error(w, "Unsupported media type", http.StatusBadRequest)
					return
				}
				q.Types = []frontend.MediaType{"movie", "episode"}
			}
		}
	}
	if personalCollectionCount(shelf.row) > 0 {
		if latest {
			i.writePersonalCollections(w, r, shelf, q, true)
		} else {
			i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
		}
		return
	}
	page, total, err := i.personalTitlePage(r, v, shelf, q, latest)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	i.notePersonalDelivery(r, v, shelf, page)
	i.writeListedTitles(w, withRankedRow(withPreviewRowArtwork(r, shelf.row.PreviewOnFocus), shelf.row.RankedTop10, page, offset), v, page, nil, total, true, offset, latest)
}

func (i *instance) personalViewOrderWithViews(r *http.Request, v *frontend.Viewer, views []frontend.View) []string {
	shelves := i.personalShelves(r, v)
	if len(shelves) == 0 || i.host.Experience == nil {
		return nil
	}
	order := make([]string, 0, len(shelves)+len(views))
	for _, shelf := range shelves {
		order = append(order, shelf.wireID)
	}
	for _, view := range views {
		n, err := ids.ParseDecimal(view.ID)
		if err != nil || ids.PersonalView(n) {
			continue
		}
		wire, err := ids.GUID(ids.View, n)
		if err == nil {
			order = append(order, wire)
		}
	}
	return order
}

const subsetLatestCompareLimit = 12

func latestTitleIdentity(t frontend.Title) frontend.RatingKey {
	if (t.Type == frontend.TypeEpisode || t.Type == frontend.TypeSeason) && t.ShowKey != 0 {
		return t.ShowKey
	}
	return t.Key
}

// redundantSubsetViews reports which rule/catalogue subset views repeat their
// parent library's Latest Home row without differing meaningfully.
func (i *instance) redundantSubsetViews(r *http.Request, v *frontend.Viewer, views []frontend.View, hidePlayed bool) map[string]bool {
	if v == nil || i.host.Experience == nil || len(views) < 2 {
		return nil
	}
	parents := frontend.SubsetParents(views)
	if len(parents) == 0 {
		return nil
	}
	activity, ok := i.host.Experience.(frontend.LibraryActivity)
	if !ok {
		return nil
	}
	ctx := frontend.WithLatestPlayed(r.Context(), !hidePlayed)
	dev := device(r)
	loadLatest := func(viewID string) ([]frontend.LatestItem, error) {
		if grouped, ok := i.host.Experience.(frontend.LibraryActivityGrouping); ok {
			return grouped.LatestAddedGrouped(ctx, v, dev, viewID, subsetLatestCompareLimit, true)
		}
		return activity.LatestAdded(ctx, v, dev, viewID, subsetLatestCompareLimit)
	}
	parentCache := map[string]map[frontend.RatingKey]bool{}
	var redundant map[string]bool
	for _, view := range views {
		parentID, isSubset := parents[view.ID]
		if !isSubset {
			continue
		}
		parentKeys, cached := parentCache[parentID]
		if !cached {
			pItems, err := loadLatest(parentID)
			if err != nil {
				parentCache[parentID] = nil
				continue
			}
			parentKeys = make(map[frontend.RatingKey]bool, len(pItems))
			for _, it := range pItems {
				if k := latestTitleIdentity(it.Title); k != 0 {
					parentKeys[k] = true
				}
			}
			parentCache[parentID] = parentKeys
		}
		if parentKeys == nil {
			continue
		}
		cItems, err := loadLatest(view.ID)
		if err != nil {
			continue
		}
		cKeys := make([]frontend.RatingKey, 0, len(cItems))
		for _, it := range cItems {
			if k := latestTitleIdentity(it.Title); k != 0 {
				cKeys = append(cKeys, k)
			}
		}
		if !frontend.SubsetRowDistinct(cKeys, parentKeys, nil) {
			if redundant == nil {
				redundant = map[string]bool{}
			}
			redundant[view.ID] = true
		}
	}
	return redundant
}

func (i *instance) redundantSubsetLatestExcludes(r *http.Request, v *frontend.Viewer, views []frontend.View, hidePlayed bool) []string {
	if v == nil || i.host.Experience == nil {
		return nil
	}
	if views == nil {
		var err error
		views, err = i.host.Experience.Views(r.Context(), v)
		if err != nil {
			return nil
		}
	}
	redundant := i.redundantSubsetViews(r, v, views, hidePlayed)
	if len(redundant) == 0 {
		return nil
	}
	out := make([]string, 0, len(redundant))
	for _, view := range views {
		if !redundant[view.ID] {
			continue
		}
		n, err := ids.ParseDecimal(view.ID)
		if err != nil || ids.PersonalView(n) {
			continue
		}
		if wire, err := ids.GUID(ids.View, n); err == nil {
			out = append(out, wire)
		}
	}
	return out
}

const (
	userConfigClient = "mediabrowser"
	userConfigKey    = "user-configuration"
)

func (i *instance) storedUserConfig(r *http.Request, v *frontend.Viewer) (map[string]any, bool) {
	if i.host.Preferences == nil || v == nil {
		return nil, false
	}
	body, err := i.host.Preferences.Get(r.Context(), v, userConfigClient, userConfigKey)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	var object map[string]any
	if json.Unmarshal(body, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

func (i *instance) applyOrderedViews(r *http.Request, v *frontend.Viewer, configuration map[string]any) {
	if stored, ok := i.storedUserConfig(r, v); ok {
		for _, key := range []string{"OrderedViews", "LatestItemsExcludes", "MyMediaExcludes", "HidePlayedInLatest"} {
			value, exists := stored[key]
			if !exists {
				continue
			}
			if list, ok := value.([]any); ok && i.protocol == "jellyfin" {
				// jellyfin-sdk-kotlin reads these as UUID lists; one id it
				// cannot parse fails the whole user response.
				value = uuidStrings(list)
			}
			configuration[key] = value
		}
		if _, hasExcludes := stored["LatestItemsExcludes"]; !hasExcludes {
			hidePlayed := true
			if hp, ok := stored["HidePlayedInLatest"].(bool); ok {
				hidePlayed = hp
			}
			if excludes := i.redundantSubsetLatestExcludes(r, v, nil, hidePlayed); len(excludes) > 0 {
				configuration["LatestItemsExcludes"] = excludes
			}
		}
		return
	}
	var views []frontend.View
	if i.host.Experience != nil && v != nil {
		views, _ = i.host.Experience.Views(r.Context(), v)
	}
	if order := i.personalViewOrderWithViews(r, v, views); len(order) > 0 {
		configuration["OrderedViews"] = order
	}
	if excludes := i.redundantSubsetLatestExcludes(r, v, views, true); len(excludes) > 0 {
		configuration["LatestItemsExcludes"] = excludes
	}
}

func (i *instance) saveUserConfiguration(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		if errors.Is(err, errViewerScope) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var object map[string]any
	if decode(r, &object) != nil || object == nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	_, hasMode := object["SubtitleMode"]
	_, hasLanguage := object["SubtitleLanguagePreference"]
	home := false
	for _, key := range []string{"OrderedViews", "LatestItemsExcludes", "MyMediaExcludes", "HidePlayedInLatest"} {
		if _, ok := object[key]; ok {
			home = true
			break
		}
	}
	if home && i.host.Preferences == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if hasMode || hasLanguage {
		if err = i.saveSubtitleConfiguration(r, v, object); err != nil {
			if errors.Is(err, errBadSubtitle) {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	kept := map[string]any{}
	if stored, ok := i.storedUserConfig(r, v); ok {
		for key, value := range stored {
			kept[key] = value
		}
	}
	for _, key := range []string{"OrderedViews", "LatestItemsExcludes", "MyMediaExcludes"} {
		value, exists := object[key]
		if !exists {
			continue
		}
		if !stringList(value) {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		kept[key] = value
	}
	if value, exists := object["HidePlayedInLatest"]; exists {
		if _, ok := value.(bool); !ok {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		kept["HidePlayedInLatest"] = value
	}
	if !home {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := json.Marshal(kept)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if err = i.host.Preferences.Put(r.Context(), v, userConfigClient, userConfigKey, body); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func stringList(value any) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		text, ok := item.(string)
		if !ok || len(text) > 128 {
			return false
		}
	}
	return true
}

func (i *instance) applyHomeSectionDefaults(r *http.Request, v *frontend.Viewer, id string, object map[string]any) {
	if !strings.EqualFold(id, "usersettings") || v == nil {
		return
	}
	prefs, _ := object["CustomPrefs"].(map[string]any)
	if prefs == nil {
		prefs = map[string]any{}
		object["CustomPrefs"] = prefs
	}
	for key := range prefs {
		if strings.HasPrefix(strings.ToLower(key), "homesection") {
			return
		}
	}
	if len(i.personalShelves(r, v)) == 0 {
		return
	}
	defaults := []string{"latestmedia", "resume", "nextup", "smalllibrarytiles", "none", "none", "none", "none", "none", "none"}
	for index, value := range defaults {
		prefs["homesection"+strconv.Itoa(index)] = value
	}
}

func (i *instance) recommendations(w http.ResponseWriter, r *http.Request, want frontend.MediaType) {
	r = withJellyfinListFiles(withBrowseMemo(withListMediaEstimate(withSeriesStudioCache(r))))
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	viewID, scoped, valid := parseLibraryParentView(r)
	if !valid {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	// Jellyfin and Emby answer five categories of eight items unless asked
	// otherwise (categoryLimit = 5, itemLimit = 8 on both servers).
	categoryLimit := queryInt(r, "CategoryLimit", 5)
	itemLimit := queryInt(r, "ItemLimit", queryInt(r, "Limit", 8))
	var shelves []personalShelf
	if scoped {
		if viewID == "" {
			jsonReply(w, []any{})
			return
		}
		shelves, err = i.loadPersonalLibraryShelves(r.Context(), v, device(r), viewID, categoryLimit)
		if errors.Is(err, frontend.ErrNotFound) {
			jsonReply(w, []any{})
			return
		}
		if err != nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
	} else {
		shelves = i.loadPersonalShelves(r.Context(), v, device(r))
	}
	type category struct {
		shelf  personalShelf
		titles []frontend.Title
	}
	selected := make([]category, 0, min(max(categoryLimit, 0), len(shelves)))
	var allTitles, unranked []frontend.Title
	var keys, genreKeys []frontend.RatingKey
	seen := map[frontend.RatingKey]bool{}
	for _, shelf := range shelves {
		if len(selected) >= categoryLimit {
			break
		}
		titles := make([]frontend.Title, 0, len(shelf.titles))
		for _, title := range shelf.titles {
			if recommendationTypeMatches(want, title.Type) {
				titles = append(titles, title)
			}
		}
		if len(titles) == 0 {
			continue
		}
		if shelf.row.RankedTop10 && len(titles) > 10 {
			titles = titles[:10]
		}
		if itemLimit >= 0 && len(titles) > itemLimit {
			titles = titles[:itemLimit]
		}
		selected = append(selected, category{shelf, titles})
		if !shelf.row.RankedTop10 {
			unranked = append(unranked, titles...)
		}
		for _, title := range titles {
			if seen[title.Key] {
				continue
			}
			seen[title.Key] = true
			keys = append(keys, title.Key)
			allTitles = append(allTitles, title)
			if len(title.Genres) > 0 {
				genreKeys = append(genreKeys, title.Key)
			}
		}
	}
	if len(selected) == 0 {
		jsonReply(w, []any{})
		return
	}
	states, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if facets, ok := i.host.Experience.(interface {
		PrefetchNamedFacetIDs(context.Context, *frontend.Viewer, []frontend.RatingKey, string) error
	}); ok && len(genreKeys) > 0 && (fieldSet(r)["genres"] || !jellyfinListFile(r)) {
		_ = facets.PrefetchNamedFacetIDs(r.Context(), v, genreKeys, "genre")
	}
	r = i.noteLocalUnplayedCounts(r, v, allTitles)
	if i.protocol == "jellyfin" {
		i.prefetchSeriesFacts(r, v, allTitles)
	}
	i.prefetchParentArtwork(r, allTitles)
	ratingFacts := i.pageRatingFacts(r, v, allTitles)
	i.primeTitleLibraries(r, v, allTitles)
	unrankedImages := i.gridImagesFor(r, unranked)
	categories := make([]any, 0, len(selected))
	for _, category := range selected {
		shelf, titles := category.shelf, category.titles
		i.notePersonalDelivery(r, v, shelf, titles)
		items := make([]any, 0, len(titles))
		images := unrankedImages
		if shelf.row.RankedTop10 {
			images = i.gridImagesFor(withRankedRow(r, true, titles, 0), titles)
		}
		for _, title := range titles {
			item := i.itemDTO(title, states[title.Key])
			for key, value := range images[title.Key] {
				item[key] = value
			}
			if !shelf.row.RankedTop10 {
				previewRowArtwork(withPreviewRowArtwork(r, shelf.row.PreviewOnFocus), item)
			}
			i.shapeJellyfinItem(r, v, item, title)
			i.applyListedRatings(item, ratingFacts, title.Key)
			items = append(items, item)
		}
		kind, baseline := recommendationWire(shelf.row)
		categories = append(categories, map[string]any{
			"BaselineItemName": baseline, "CategoryId": shelf.wireID, "Items": items, "RecommendationType": kind,
		})
	}
	jsonReply(w, categories)
}

func recommendationTypeMatches(want, got frontend.MediaType) bool {
	if want == "movie" {
		return got == "movie"
	}
	return got == "show" || got == "season" || got == "episode"
}

func recommendationWire(row frontend.Row) (string, string) {
	title := strings.TrimSpace(row.Title)
	switch row.Kind {
	case "because-you-watched":
		return "SimilarToRecentlyPlayed", recommendationBaseline(title, "because you watched ", "because you've watched ")
	case "favorites":
		return "SimilarToLikedItem", recommendationBaseline(title, "because you liked ", "because you like ")
	case "more-from-person":
		if strings.Contains(strings.ToLower(title), "director") {
			return "HasDirectorFromRecentlyPlayed", recommendationBaseline(title, "more from ")
		}
		return "HasActorFromRecentlyPlayed", recommendationBaseline(title, "more from ")
	default:
		if strings.Contains(strings.ToLower(title), "director") {
			return "HasDirectorFromRecentlyPlayed", title
		}
		if title == "" {
			title = personalLibraryName(row)
		}
		return "SimilarToLikedItem", title
	}
}

func recommendationBaseline(title string, prefixes ...string) string {
	if seed, ok := personalSeed(title, prefixes...); ok {
		seed = strings.TrimSuffix(seed, " and Similar")
		if seed != "" {
			return seed
		}
	}
	if title == "" {
		return "For You"
	}
	return title
}

type playCredits struct {
	mu    sync.Mutex
	items map[string]string
}

func playCreditKey(viewer int64, device, item string) string {
	return strconv.FormatInt(viewer, 10) + "\n" + device + "\n" + item
}

func (c *playCredits) remember(viewer int64, device, item, handle string) {
	if handle == "" || item == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]string{}
	}
	if len(c.items) >= playCreditCap {
		c.items = map[string]string{}
	}
	c.items[playCreditKey(viewer, device, item)] = handle
}

func (c *playCredits) take(viewer int64, device, item string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	handle, ok := c.items[playCreditKey(viewer, device, item)]
	if ok {
		delete(c.items, playCreditKey(viewer, device, item))
	}
	return handle, ok
}

func (i *instance) creditPlay(r *http.Request, v *frontend.Viewer, itemID string) {
	if v == nil || itemID == "" {
		return
	}
	handle, ok := i.credits.take(v.ID, device(r).ID, itemID)
	if !ok {
		return
	}
	reporter, ok := i.host.Experience.(frontend.RowInteractionReporter)
	if !ok {
		return
	}
	err := reporter.ReportRowInteraction(r.Context(), frontend.RowInteraction{Viewer: v, Device: device(r), Protocol: i.protocol, Handle: handle, Kind: frontend.RowPlay})
	if err != nil && i.host.Log != nil {
		i.host.Log.Warn("row play failed", "error", err)
	}
}

// uuidStrings keeps the entries a Jellyfin client can read as UUIDs: 32 hex
// digits, optionally in the dashed 8-4-4-4-12 form.
func uuidStrings(list []any) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, _ := item.(string)
		plain := text
		if len(text) == 36 && text[8] == '-' && text[13] == '-' && text[18] == '-' && text[23] == '-' {
			plain = text[:8] + text[9:13] + text[14:18] + text[19:23] + text[24:]
		}
		if len(plain) != 32 {
			continue
		}
		if _, err := hex.DecodeString(plain); err == nil {
			out = append(out, text)
		}
	}
	return out
}
