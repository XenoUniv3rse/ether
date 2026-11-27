package livetv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// redactedSecret is what the owner API shows in place of a stored secret.
// Sending it back unchanged keeps the stored value (same convention as the
// admin config API).
const redactedSecret = "••••"

// SourceAuth is how Ether authenticates to an IPTV or guide provider. It is
// used for the source's playlist and guide fetches and, through
// upstreamHeaders, for the streams of channel inputs that belong to it.
type SourceAuth struct {
	// Type: "" (none), "basic", "bearer", "headers" or "xtream".
	Type     string `json:"type,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` // secret
	Token    string `json:"token,omitempty"`    // secret (bearer)
	// UserAgent and Referer apply to every type; many providers reject
	// unknown clients.
	UserAgent string `json:"userAgent,omitempty"`
	Referer   string `json:"referer,omitempty"`
	// Headers are extra request headers (cookies, API keys); values are secret.
	Headers map[string]string `json:"headers,omitempty"`
	// Xtream Codes: the panel URL. Playlist and guide URLs are derived from
	// it and the credentials, and never returned by the API.
	Server string `json:"server,omitempty"`
	// Output selects the Xtream stream format: "ts" (default) or "m3u8".
	Output string `json:"output,omitempty"`
}

var authTypes = map[string]bool{"": true, "basic": true, "bearer": true, "headers": true, "xtream": true}

// forbiddenHeaders are managed by the HTTP stack or by the proxy itself.
var forbiddenHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true,
	"Range": true, "If-Range": true, "Te": true, "Upgrade": true, "X-Plex-Token": true,
}

func (a *SourceAuth) empty() bool {
	return a == nil || (a.Type == "" && a.UserAgent == "" && a.Referer == "" && len(a.Headers) == 0)
}

func (a *SourceAuth) validate() error {
	if a == nil {
		return nil
	}
	if !authTypes[a.Type] {
		return fmt.Errorf("unknown source authentication %q", a.Type)
	}
	switch a.Type {
	case "basic":
		if a.Username == "" {
			return fmt.Errorf("basic authentication needs a username")
		}
	case "bearer":
		if a.Token == "" {
			return fmt.Errorf("bearer authentication needs a token")
		}
	case "headers":
		if len(a.Headers) == 0 {
			return fmt.Errorf("add at least one header")
		}
	case "xtream":
		if !validHTTP(a.Server) {
			return fmt.Errorf("the Xtream server must be an HTTP(S) URL")
		}
		if a.Username == "" || a.Password == "" {
			return fmt.Errorf("Xtream needs a username and password")
		}
		if a.Output != "" && a.Output != "ts" && a.Output != "m3u8" {
			return fmt.Errorf("Xtream output must be ts or m3u8")
		}
	}
	if len(a.Headers) > 32 {
		return fmt.Errorf("at most 32 custom headers")
	}
	for k, v := range a.Headers {
		name := http.CanonicalHeaderKey(strings.TrimSpace(k))
		if name == "" || strings.ContainsAny(name, " :\r\n") || forbiddenHeaders[name] {
			return fmt.Errorf("header %q can't be set on a source", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %q has an invalid value", k)
		}
	}
	if strings.ContainsAny(a.UserAgent+a.Referer, "\r\n") {
		return fmt.Errorf("invalid user agent or referer")
	}
	return nil
}

// header renders the request headers for this provider.
func (a *SourceAuth) header() http.Header {
	h := http.Header{}
	if a == nil {
		return h
	}
	if a.UserAgent != "" {
		h.Set("User-Agent", a.UserAgent)
	} else if a.Type == "xtream" {
		// One identity for the panel's API, guide and streams: panels that
		// bind a login to a device compare them.
		h.Set("User-Agent", defaultUserAgent())
	}
	if a.Referer != "" {
		h.Set("Referer", a.Referer)
	}
	for k, v := range a.Headers {
		h.Set(strings.TrimSpace(k), v)
	}
	switch a.Type {
	case "basic":
		r := &http.Request{Header: http.Header{}}
		r.SetBasicAuth(a.Username, a.Password)
		h.Set("Authorization", r.Header.Get("Authorization"))
	case "bearer":
		h.Set("Authorization", "Bearer "+a.Token)
	}
	return h
}

// xtreamURLs derives an Xtream Codes panel's lineup (its JSON API, see
// xtream.go) and XMLTV URLs.
func (a *SourceAuth) xtreamURLs() (lineup, guide string) {
	base := strings.TrimRight(strings.TrimSpace(a.Server), "/")
	q := url.Values{"username": {a.Username}, "password": {a.Password}}
	return base + "/player_api.php?" + q.Encode(), base + "/xmltv.php?" + q.Encode()
}

// defaultUserAgent names Ether to providers when the owner sets no user
// agent. Panels commonly refuse Go's generic client name outright.
func defaultUserAgent() string { return sourceUserAgent }

// redacted returns a copy safe for the owner API.
func (a *SourceAuth) redacted() *SourceAuth {
	if a == nil {
		return nil
	}
	c := *a
	if c.Password != "" {
		c.Password = redactedSecret
	}
	if c.Token != "" {
		c.Token = redactedSecret
	}
	if len(a.Headers) > 0 {
		c.Headers = make(map[string]string, len(a.Headers))
		for k, v := range a.Headers {
			if v != "" {
				v = redactedSecret
			}
			c.Headers[k] = v
		}
	}
	return &c
}

// keepSecrets replaces redacted placeholders with the stored secrets.
func (a *SourceAuth) keepSecrets(old *SourceAuth) {
	if a == nil {
		return
	}
	if old == nil {
		old = &SourceAuth{}
	}
	if a.Password == redactedSecret {
		a.Password = old.Password
	}
	if a.Token == redactedSecret {
		a.Token = old.Token
	}
	for k, v := range a.Headers {
		if v == redactedSecret {
			a.Headers[k] = old.Headers[k]
		}
	}
}

// hasRedacted reports whether a carries a redacted placeholder to restore.
func (a *SourceAuth) hasRedacted() bool {
	if a.Password == redactedSecret || a.Token == redactedSecret {
		return true
	}
	for _, v := range a.Headers {
		if v == redactedSecret {
			return true
		}
	}
	return false
}

// prepareSourceAuth runs before an import: it restores secrets the owner
// API sent back redacted, validates the settings and derives Xtream URLs.
type xtreamParsedURL struct {
	Server   string
	Username string
	Password string
	Output   string
}

func xtreamValidCreds(user, pass string) bool {
	return user != "" && pass != "" && user != redactedSecret && pass != redactedSecret
}

func xtreamOutputHint(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "m3u8", "hls":
		return "m3u8"
	case "ts", "mpegts":
		return "ts"
	default:
		return ""
	}
}

func xtreamSegmentOutputHint(seg string) string {
	lower := strings.ToLower(seg)
	switch {
	case strings.HasSuffix(lower, ".m3u8"):
		return "m3u8"
	case strings.HasSuffix(lower, ".ts"):
		return "ts"
	default:
		return ""
	}
}

func xtreamQueryParam(q url.Values, name string) string {
	if v := q.Get(name); v != "" {
		return v
	}
	for k, vs := range q {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

func xtreamJoinBase(origin string, segs []string) string {
	if len(segs) == 0 {
		return origin
	}
	return origin + "/" + strings.Join(segs, "/")
}

func isXtreamPathFamily(seg string) bool {
	switch strings.ToLower(seg) {
	case "live", "movie", "series", "timeshift":
		return true
	default:
		return false
	}
}

// parseXtreamURL splits a pasted Xtream provider link (get.php, player_api.php,
// xmltv.php, /live/U/P/..., or /U/P/...) into server, username, password, and
// optional stream format.
func parseXtreamURL(raw string) (xtreamParsedURL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return xtreamParsedURL{}, false
	}
	origin := strings.ToLower(u.Scheme) + "://" + u.Host
	var rawSegs []string
	for _, part := range strings.Split(u.EscapedPath(), "/") {
		if part != "" {
			rawSegs = append(rawSegs, part)
		}
	}
	q := u.Query()
	qUser := xtreamQueryParam(q, "username")
	qPass := xtreamQueryParam(q, "password")
	qOut := xtreamOutputHint(xtreamQueryParam(q, "output"))

	if xtreamValidCreds(qUser, qPass) {
		baseSegs := rawSegs
		if len(baseSegs) > 0 && strings.HasSuffix(strings.ToLower(baseSegs[len(baseSegs)-1]), ".php") {
			baseSegs = baseSegs[:len(baseSegs)-1]
		}
		return xtreamParsedURL{
			Server:   xtreamJoinBase(origin, baseSegs),
			Username: qUser,
			Password: qPass,
			Output:   qOut,
		}, true
	}

	for i := 0; i+3 <= len(rawSegs); i++ {
		if !isXtreamPathFamily(rawSegs[i]) {
			continue
		}
		user, err1 := url.PathUnescape(rawSegs[i+1])
		pass, err2 := url.PathUnescape(rawSegs[i+2])
		if err1 != nil || err2 != nil || !xtreamValidCreds(user, pass) {
			return xtreamParsedURL{}, false
		}
		out := qOut
		if out == "" && len(rawSegs) > i+3 {
			out = xtreamSegmentOutputHint(rawSegs[len(rawSegs)-1])
		}
		return xtreamParsedURL{
			Server:   xtreamJoinBase(origin, rawSegs[:i]),
			Username: user,
			Password: pass,
			Output:   out,
		}, true
	}

	if len(rawSegs) >= 3 && !strings.HasSuffix(strings.ToLower(rawSegs[len(rawSegs)-1]), ".php") {
		user, err1 := url.PathUnescape(rawSegs[0])
		pass, err2 := url.PathUnescape(rawSegs[1])
		if err1 == nil && err2 == nil && xtreamValidCreds(user, pass) {
			out := qOut
			if out == "" {
				out = xtreamSegmentOutputHint(rawSegs[len(rawSegs)-1])
			}
			return xtreamParsedURL{
				Server:   origin,
				Username: user,
				Password: pass,
				Output:   out,
			}, true
		}
	}

	if u.User != nil {
		if pass, ok := u.User.Password(); ok && xtreamValidCreds(u.User.Username(), pass) {
			baseSegs := rawSegs
			if len(baseSegs) > 0 && strings.HasSuffix(strings.ToLower(baseSegs[len(baseSegs)-1]), ".php") {
				baseSegs = baseSegs[:len(baseSegs)-1]
			}
			return xtreamParsedURL{
				Server:   xtreamJoinBase(origin, baseSegs),
				Username: u.User.Username(),
				Password: pass,
				Output:   qOut,
			}, true
		}
	}

	return xtreamParsedURL{}, false
}

// stripXtreamServerURL removes query parameters, fragments, userinfo, and
// trailing .php endpoints so credentials or query tokens never stay in Server.
func stripXtreamServerURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return trimmed
	}
	origin := strings.ToLower(u.Scheme) + "://" + u.Host
	var rawSegs []string
	for _, part := range strings.Split(u.EscapedPath(), "/") {
		if part != "" {
			rawSegs = append(rawSegs, part)
		}
	}
	if len(rawSegs) > 0 && strings.HasSuffix(strings.ToLower(rawSegs[len(rawSegs)-1]), ".php") {
		rawSegs = rawSegs[:len(rawSegs)-1]
	}
	for i := 0; i+3 <= len(rawSegs); i++ {
		if isXtreamPathFamily(rawSegs[i]) {
			return xtreamJoinBase(origin, rawSegs[:i])
		}
	}
	if len(rawSegs) >= 3 {
		for _, seg := range rawSegs[:2] {
			if uSeg, _ := url.PathUnescape(seg); uSeg == redactedSecret {
				return origin
			}
		}
	}
	return xtreamJoinBase(origin, rawSegs)
}

func (a *SourceAuth) normalizeXtream() {
	if a == nil || a.Type != "xtream" {
		return
	}
	if parsed, ok := parseXtreamURL(a.Server); ok {
		a.Server = parsed.Server
		if strings.TrimSpace(a.Username) == "" || a.Password == "" || a.Password == redactedSecret {
			if strings.TrimSpace(a.Username) == "" || a.Password == redactedSecret {
				a.Username = parsed.Username
			}
		}
		if a.Password == "" || a.Password == redactedSecret {
			a.Password = parsed.Password
		}
		if a.Output == "" && parsed.Output != "" {
			a.Output = parsed.Output
		}
		return
	}
	a.Server = stripXtreamServerURL(a.Server)
}

func (s *Service) prepareSourceAuth(ctx context.Context, src *Source) error {
	if err := s.prepareLogins(ctx, src); err != nil {
		return err
	}
	if src.Auth.empty() {
		src.Auth = nil
		return nil
	}
	src.Auth.normalizeXtream()
	old, err := s.storedSourceByID(ctx, src.ID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	if src.Auth.hasRedacted() && old.Auth != nil && sourceMoved(src, old) {
		return errSourceSecretMoved
	}
	src.Auth.keepSecrets(old.Auth)
	if err := src.Auth.validate(); err != nil {
		return err
	}
	if src.Auth.Type == "xtream" {
		playlist, guide := src.Auth.xtreamURLs()
		src.PlaylistURL, src.Playlist = playlist, ""
		// An owner-chosen guide URL wins over the panel's XMLTV export.
		if src.GuideURL == "" || isXtreamURL(src.GuideURL, src.Auth.Server) {
			src.GuideURL, src.Guide = guide, ""
		}
	}
	return nil
}

func isXtreamURL(raw, server string) bool {
	return strings.HasPrefix(raw, strings.TrimRight(server, "/")+"/xmltv.php")
}

// redactSourceAuth hides secrets (and Xtream URLs, which embed them) from a
// source about to be returned by the owner API.
func redactSourceAuth(src *Source) {
	src.Logins = append([]ProviderLogin(nil), src.Logins...)
	for i := range src.Logins {
		l := &src.Logins[i]
		if l.Password != "" {
			l.Password = redactedSecret
		}
		if l.PlaylistURL != "" {
			l.PlaylistURL = redactedSecret
		}
	}
	if src.Kind == legacyStremioKind && src.PlaylistURL != "" {
		src.PlaylistURL = redactedSecret
	}
	if src.Auth == nil {
		return
	}
	if src.Auth.Type == "xtream" {
		src.PlaylistURL = ""
		if isXtreamURL(src.GuideURL, src.Auth.Server) {
			src.GuideURL = ""
		}
	}
	src.Auth = src.Auth.redacted()
}

// readURLWith fetches a source document with the provider's headers.
// kind names the document in limit and timeout errors ("playlist" or "guide").
func (s *Service) readURLWith(ctx context.Context, raw string, max int64, h http.Header, kind string) (string, error) {
	rc, err := s.openURL(ctx, raw, h)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := readLimited(rc, max, kind)
	if err != nil {
		return "", sourceReadFailure(err, kind)
	}
	return string(b), nil
}

// sourceDocumentTimeout bounds the wait for a source to start answering. An
// Xtream panel composes a playlist or guide on demand, so the wait can be
// minutes; once bytes flow, sourceIdleTimeout bounds a silent body and the
// document may take as long as it needs.
const sourceDocumentTimeout = refreshTimeout

// sourceIdleTimeout is how long a document may go silent mid-transfer before
// the import fails.
const sourceIdleTimeout = 2 * time.Minute

// errSourceStalled marks a document whose body stopped flowing.
var errSourceStalled = errors.New("source stopped sending data")

// documentClient returns the client for a source document. It keeps the
// service client's transport (and so its live egress routing) but has no total
// timeout: openURL bounds the header wait and a silent body itself.
func (s *Service) documentClient() *http.Client {
	c := *s.o.HTTPClient
	c.Timeout = 0
	return &c
}

// sourceBody counts a document's bytes for the progress endpoint and cancels
// a body that stops flowing.
type sourceBody struct {
	io.ReadCloser
	timer    *time.Timer
	cancel   context.CancelFunc
	stalled  *atomic.Bool
	progress *progressTracker
}

func (b *sourceBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(sourceIdleTimeout)
		if b.progress != nil {
			b.progress.addRead(int64(n))
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			b.timer.Stop()
			return n, err
		}
		if b.stalled.Load() {
			return n, errSourceStalled
		}
		b.timer.Stop()
	}
	return n, err
}

func (b *sourceBody) Close() error {
	b.timer.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

// sourceFetchFailure explains a transport failure without echoing the request
// URL: source URLs, Xtream's derived ones in particular, carry the provider's
// credentials, and callers store and surface these errors. The cause itself
// (DNS, refused connection, TLS, timeout) is what tells an owner how to fix
// the source.
func sourceFetchFailure(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("source fetch failed: the source did not answer within %s", sourceDocumentTimeout)
	}
	return fmt.Errorf("source fetch failed: %w", err)
}

// sourceReadFailure wraps a body that stalled or timed out in the same
// sentence as a failed fetch. Any other error (a size limit, malformed XML)
// already says what went wrong.
func sourceReadFailure(err error, kind string) error {
	if errors.Is(err, errSourceStalled) {
		return fmt.Errorf("source fetch failed: the source stopped sending the %s for %s", kind, sourceIdleTimeout)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("source fetch failed: the source did not finish sending the %s within %s", kind, sourceDocumentTimeout)
	}
	return err
}

// openURL fetches a source document and leaves the body to the caller. The
// header wait is sourceDocumentTimeout and a silent body is sourceIdleTimeout;
// a document that keeps sending data may take as long as it needs.
func (s *Service) openURL(ctx context.Context, raw string, h http.Header) (io.ReadCloser, error) {
	if !validHTTP(raw) {
		return nil, fmt.Errorf("source must use HTTP(S)")
	}
	ctx, cancel := context.WithCancel(ctx)
	var stalled atomic.Bool
	timer := time.AfterFunc(sourceDocumentTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, sourceFetchFailure(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUserAgent())
	}
	res, err := s.documentClient().Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		if stalled.Load() {
			return nil, fmt.Errorf("source fetch failed: the source did not answer within %s", sourceDocumentTimeout)
		}
		return nil, sourceFetchFailure(err)
	}
	closeBody := func() {
		timer.Stop()
		cancel()
		res.Body.Close()
	}
	switch {
	case res.StatusCode == 401 || res.StatusCode == 403:
		closeBody()
		return nil, &providerAuthError{StatusCode: res.StatusCode}
	case res.StatusCode != 200:
		closeBody()
		return nil, fmt.Errorf("source returned HTTP %d", res.StatusCode)
	}
	if stalled.Load() {
		closeBody()
		return nil, fmt.Errorf("source fetch failed: the source did not answer within %s", sourceDocumentTimeout)
	}
	timer.Reset(sourceIdleTimeout)
	body := &sourceBody{ReadCloser: res.Body, timer: timer, cancel: cancel, stalled: &stalled}
	if p := importProgress(ctx); p != nil {
		p.setTotal(res.ContentLength)
		body.progress = p
	}
	return body, nil
}

// readGuide streams a guide URL, gunzipping when the body is gzip, and keeps
// only the filtered channel and programme entries.
func (s *Service) readGuide(ctx context.Context, raw string, h http.Header, f guideFilter) (XMLGuide, error) {
	rc, err := s.openURL(ctx, raw, h)
	if err != nil {
		return XMLGuide{}, err
	}
	defer rc.Close()
	rd, err := maybeGunzip(rc, maxGuideBytes, "guide")
	if err != nil {
		return XMLGuide{}, sourceReadFailure(err, "guide")
	}
	defer closeReader(rd)
	g, _, err := parseXMLTV(rd, f)
	if err != nil {
		err = sourceReadFailure(err, "guide")
	}
	return g, err
}

// authCache keeps provider settings for the stream path, which asks for them
// on every segment. Imports and deletes drop the entry and advance gen so an
// in-flight database read from before the mutation never repopulates stale data.
type authCache struct {
	mu  sync.Mutex
	gen uint64
	m   map[string]authEntry
}

type authEntry struct {
	name          string
	playlistHost  string
	unratedRating string
	adultGroups   map[string]bool
	pool          *Source
	account       *ProviderAccount
	auth          *SourceAuth
	limit         int           // connection limit; 0 is none
	gap           time.Duration // least time between new provider streams
	perHour       int           // new provider streams per hour; 0 is no cap
	at            time.Time
	// disabled caches a paused source, so channel listings and policy checks
	// need no database read per channel. forgetSourceAuth clears it.
	disabled bool
}

const authCacheTTL = 30 * time.Second

func (s *Service) forgetSourceAuth(id string) {
	c := &s.authCache
	c.mu.Lock()
	c.gen++
	delete(c.m, id)
	c.mu.Unlock()
	s.bumpHealthRevision()
}

func (s *Service) setSourceEntry(id string, e authEntry) {
	c := &s.authCache
	c.mu.Lock()
	c.gen++
	if c.m == nil {
		c.m = map[string]authEntry{}
	}
	c.m[id] = e
	c.mu.Unlock()
	s.bumpHealthRevision()
}

func (s *Service) sourceAuth(ctx context.Context, id string) (*SourceAuth, error) {
	e, err := s.sourceEntry(ctx, id)
	return e.auth, err
}

// sourceConnectionLimit is how many connections the source's provider
// allows at once (an Xtream panel's max_connections); 0 means no limit.
func (s *Service) sourceConnectionLimit(id string) int {
	e, err := s.sourceEntry(s.serviceContext(), id)
	if err != nil {
		return 0
	}
	return e.limit
}

// sourcePaused reports whether the owner disabled the source that owns c.
// A paused source keeps its settings and channels, but its channels are not
// listed, guided or tuned until it is enabled again.
func (s *Service) sourcePaused(ctx context.Context, c Channel) bool {
	if c.SourceID == "" {
		return false
	}
	_, err := s.sourceEntry(ctx, c.SourceID)
	if !errors.Is(err, core.ErrNotAvailable) {
		return false
	}
	for _, src := range c.StreamSources {
		if src.Enabled && src.AuthSourceID != "" && src.AuthSourceID != c.SourceID && s.channelSourceEnabled(ctx, src.AuthSourceID) {
			return false
		}
	}
	return true
}

// defaultTuneGap spaces new streams on a connection-limited account: a
// viewer flicking through channels must not look like a scan.
const defaultTuneGap = 2 * time.Second

func (s *Service) sourceEntry(ctx context.Context, id string) (authEntry, error) {
	if id == "" {
		return authEntry{}, nil
	}
	c := &s.authCache
	c.mu.Lock()
	e, ok := c.m[id]
	gen := c.gen
	c.mu.Unlock()
	if ok && s.o.Now().Sub(e.at) < authCacheTTL {
		if e.disabled {
			return authEntry{}, core.ErrNotAvailable
		}
		return e, nil
	}
	src, err := s.storedSourceByID(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		src, err = Source{}, nil
	}
	if err != nil {
		return authEntry{}, err
	}
	if src.Disabled {
		c.mu.Lock()
		if c.gen == gen {
			if c.m == nil {
				c.m = map[string]authEntry{}
			}
			c.m[id] = authEntry{at: s.o.Now(), disabled: true}
		}
		c.mu.Unlock()
		if ok && !e.disabled {
			s.bumpHealthRevision()
		}
		return authEntry{}, core.ErrNotAvailable
	}
	prevEntry, hadPrev := e, ok
	e = authEntryFromSource(src, s.o.Now())
	if src.Account != nil {
		acct := *src.Account
		if src.Account.ExpiresAt != nil {
			exp := *src.Account.ExpiresAt
			acct.ExpiresAt = &exp
		}
		e.account = &acct
	}
	c.mu.Lock()
	if c.gen == gen {
		if c.m == nil {
			c.m = map[string]authEntry{}
		}
		c.m[id] = e
	} else if cur, exists := c.m[id]; exists && !cur.disabled && s.o.Now().Sub(cur.at) < authCacheTTL {
		e = cur
	}
	c.mu.Unlock()
	if hadPrev && (prevEntry.disabled != e.disabled || !sameAccountHealth(prevEntry.account, e.account)) {
		s.bumpHealthRevision()
	}
	return e, nil
}

// upstreamHeaders returns the request headers for fetching one channel input
// (its playlists, keys and segments, and ffmpeg's input). Credentials come
// from the input's AuthSourceID. Without one, only the playlist's own entry
// (the import-owned input, or a legacy channel's single input) inherits the
// channel's source; owner-added backups stay anonymous, so an input never
// receives another provider's credentials. The input's own headers (an
// add-on's proxy headers) apply last. The result is empty, never nil, when
// no authentication applies.
func (s *Service) upstreamHeaders(ctx context.Context, c Channel, input StreamSource) (http.Header, error) {
	id := input.AuthSourceID
	if id == "" && c.SourceID != "" && (input.ID == stableID(c.ID, "playlist") || len(c.StreamSources) <= 1) {
		id = c.SourceID
	}
	a, err := s.sourceAuth(ctx, id)
	if err != nil {
		return http.Header{}, err
	}
	h := a.header()
	for k, v := range input.Headers {
		h.Set(k, v)
	}
	return h, nil
}
