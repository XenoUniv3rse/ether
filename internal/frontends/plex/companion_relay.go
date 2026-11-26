package plex

import (
	"crypto/sha256"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Plex Companion server relay.
//
// A player that opens no port of its own (Plex for Samsung, Plex's HTPC app)
// long-polls its server at /player/proxy/poll, and a controller (the Plex
// phone app, Plex Web "Play on", a home automation script) sends /player/*
// commands to the server with X-Plex-Target-Client-Identifier. The server
// hands each command to the player's poll, waits for the player's answer at
// /player/proxy/response, and returns that answer to the controller. The
// player reports its status at /player/proxy/timeline, which controllers read
// with /player/timeline/poll.
//
// Wire details follow a player implementation measured against PMS: commands
// are <Command path=… commandID=…> elements whose other attributes are the
// controller's query arguments prefixed "query" (key -> queryKey); the server
// numbers commands itself; the player's answer reaches the controller byte for
// byte.
//
// Controllers and discovery are scoped to the authenticated Ether viewer.
// A player's poll and reports additionally require its registration token.
// State is local to this server process. All Companion traffic for a player
// must reach the same Ether node, including /clients on the controller.

const (
	companionPollHold        = 20 * time.Second
	companionCommandWait     = 15 * time.Second
	companionStale           = 60 * time.Second
	companionBodyLimit       = 64 << 10
	companionMaxPlayers      = 256
	companionMaxQueued       = 16
	companionViewerPlayers   = 32
	companionTimelineWaiters = 4
	companionRequestLimit    = 8 << 10
	companionFieldLimit      = 512
	companionBodyTimeout     = 5 * time.Second
	// A timeline older than this means the player is not reporting; a
	// controller's timeline poll then asks it to (timeline/subscribe).
	companionTimelineFresh = 10 * time.Second
)

type companionKey struct {
	clientID string
	userID   int64
}

type companionCommand struct {
	id        int64
	attrs     []model.Attr
	reply     chan companionReply // nil when nobody waits for the answer
	delivered bool
	answered  bool
}

type companionReply struct {
	body        []byte
	contentType string
}

type companionPlayer struct {
	token [sha256.Size]byte

	name, product, version, address            string
	deviceClass, protocolVersion, capabilities string

	lastPoll     time.Time
	queue        []*companionCommand
	pollWake     chan struct{} // owned by the single active player poll
	waiting      map[int64]*companionCommand
	timeline     companionReply
	timeAt       time.Time
	timeWake     chan struct{} // closed when a new timeline arrives
	timeWaiters  int
	subscribe    time.Time
	subscription *companionCommand
}

type companionRelay struct {
	mu          sync.Mutex
	players     map[companionKey]*companionPlayer
	nextID      int64
	stopping    chan struct{}
	requests    companionRequests
	reports     companionRequests // reserved admission for player replies and timeline updates
	pollHold    time.Duration
	commandWait time.Duration
	stale       time.Duration
	now         func() time.Time
}

type companionRequests struct {
	active                 int
	viewers                map[int64]int
	nodeLimit, viewerLimit int
}

func newCompanionRelay() *companionRelay {
	return &companionRelay{
		players:     map[companionKey]*companionPlayer{},
		stopping:    make(chan struct{}),
		requests:    companionRequests{viewers: make(map[int64]int), nodeLimit: 512, viewerLimit: 64},
		reports:     companionRequests{viewers: make(map[int64]int), nodeLimit: 64, viewerLimit: 8},
		pollHold:    companionPollHold,
		commandWait: companionCommandWait,
		stale:       companionStale,
		now:         time.Now,
	}
}

func (c *companionRelay) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.stopping:
	default:
		close(c.stopping)
		clear(c.players)
	}
}

func (c *companionRelay) admit(w http.ResponseWriter, viewer int64, report bool) func() {
	c.mu.Lock()
	select {
	case <-c.stopping:
		c.mu.Unlock()
		writeHTTPError(w, http.StatusServiceUnavailable)
		return nil
	default:
	}
	requests := &c.requests
	if report {
		requests = &c.reports
	}
	if requests.active >= requests.nodeLimit || requests.viewers[viewer] >= requests.viewerLimit {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusTooManyRequests)
		return nil
	}
	requests.active++
	requests.viewers[viewer]++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		requests.active--
		requests.viewers[viewer]--
		if requests.viewers[viewer] == 0 {
			delete(requests.viewers, viewer)
		}
	}
}

// pruneLocked drops registrations that have stopped polling, on actual use.
func (c *companionRelay) pruneLocked(now time.Time) {
	for k, p := range c.players {
		if p.pollWake == nil && now.Sub(p.lastPoll) > c.stale {
			for _, cmd := range p.waiting {
				close(cmd.reply)
			}
			close(p.timeWake)
			delete(c.players, k)
		}
	}
}

func (c *companionRelay) live(key companionKey) *companionPlayer {
	c.pruneLocked(c.now())
	return c.players[key]
}

func companionPlayerKey(pr *Request) (companionKey, bool) {
	if pr.ClientID == "" || len(pr.ClientID) > companionFieldLimit {
		return companionKey{}, false
	}
	return companionKey{clientID: pr.ClientID, userID: pr.User.ID}, true
}

// handleCompanion serves every /player/* path.
func (s *Server) handleCompanion(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	if !companionViewer(pr) {
		writeHTTPError(w, http.StatusUnauthorized)
		return
	}
	const responsePath = "/player/proxy/response"
	const timelinePath = "/player/proxy/timeline"
	report := r.URL.Path == responsePath || r.URL.Path == timelinePath
	method := http.MethodGet
	if report {
		method = http.MethodPost
	}
	if r.Method != method {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if len(r.URL.Path)+len(r.URL.RawQuery) > companionRequestLimit {
		writeHTTPError(w, http.StatusRequestURITooLong)
		return
	}
	release := s.companion.admit(w, pr.User.ID, report)
	if release == nil {
		return
	}
	defer release()

	switch r.URL.Path {
	case "/player/proxy/poll":
		s.handleCompanionPoll(w, r)
		return
	case responsePath:
		s.handleCompanionResponse(w, r)
		return
	case timelinePath:
		s.handleCompanionTimeline(w, r)
		return
	}
	target := plexParam(r, "X-Plex-Target-Client-Identifier")
	if target == "" || len(target) > companionFieldLimit || strings.HasPrefix(r.URL.Path, "/player/proxy/") {
		// Untargeted /player/* is not a server command: PMS refuses it.
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	key := companionKey{clientID: target, userID: pr.User.ID}
	if r.URL.Path == "/player/timeline/poll" {
		s.handleCompanionTimelinePoll(w, r, key)
		return
	}
	s.relayCompanionCommand(w, r, key)
}

func companionViewer(pr *Request) bool {
	return pr.User != nil && !pr.User.Delegated && !pr.Capability && pr.Token != ""
}

func (s *Server) handleCompanionPoll(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	key, ok := companionPlayerKey(pr)
	if !ok {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	name, product, version := companionName(pr), pr.Product, pr.Version
	deviceClass, protocolVersion, capabilities := q.Get("deviceClass"), q.Get("protocolVersion"), q.Get("protocolCapabilities")
	for _, field := range []string{name, product, version, deviceClass, protocolVersion, capabilities} {
		if len(field) > companionFieldLimit {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
	}

	c := s.companion
	token := sha256.Sum256([]byte(pr.Token))
	c.mu.Lock()
	now := c.now()
	c.pruneLocked(now)
	select {
	case <-c.stopping:
		c.mu.Unlock()
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	default:
	}
	p := c.players[key]
	if p == nil {
		viewerPlayers := 0
		for existing := range c.players {
			if existing.userID == key.userID {
				viewerPlayers++
			}
		}
		if len(c.players) >= companionMaxPlayers || viewerPlayers >= companionViewerPlayers {
			c.mu.Unlock()
			writeHTTPError(w, http.StatusTooManyRequests)
			return
		}
		p = &companionPlayer{token: token, waiting: make(map[int64]*companionCommand), timeWake: make(chan struct{})}
		key.clientID = strings.Clone(key.clientID)
		c.players[key] = p
	} else if p.token != token {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if p.pollWake != nil {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusTooManyRequests)
		return
	}
	p.name, p.product, p.version = strings.Clone(name), strings.Clone(product), strings.Clone(version)
	p.address = ""
	if pr.RemoteIP.IsValid() {
		p.address = pr.RemoteIP.String()
	}
	p.deviceClass, p.protocolVersion, p.capabilities = deviceClass, protocolVersion, capabilities
	p.lastPoll = now
	wake := make(chan struct{}, 1)
	p.pollWake = wake
	pending := len(p.queue) != 0
	c.mu.Unlock()

	if !pending {
		timer := time.NewTimer(c.pollHold)
		select {
		case <-wake:
		case <-timer.C:
		case <-r.Context().Done():
		case <-c.stopping:
		case <-s.ctx.Done():
		}
		timer.Stop()
	}

	// A dropped or revoked poll must leave the queue for the next poll.
	current := r.Context().Err() == nil && s.acceptanceCurrent(r)
	c.mu.Lock()
	p.pollWake = nil
	p.lastPoll = c.now()
	var cmd *companionCommand
	if current && c.players[key] == p && !s.Closed() && len(p.queue) != 0 {
		cmd = p.queue[0]
		p.queue[0] = nil
		p.queue = p.queue[1:]
		cmd.delivered = true
	}
	c.mu.Unlock()
	if !s.companionRequestCurrent(w, r, current) {
		return
	}
	out := companionPollContainer{}
	if cmd != nil {
		out.Size = 1
		out.Command = []companionCommandElement{{Attrs: cmd.attrs}}
	}
	s.write(w, r, &out)
}

func (s *Server) companionRequestCurrent(w http.ResponseWriter, r *http.Request, accepted bool) bool {
	if r.Context().Err() != nil {
		return false
	}
	select {
	case <-s.companion.stopping:
		writeHTTPError(w, http.StatusServiceUnavailable)
		return false
	case <-s.ctx.Done():
		writeHTTPError(w, http.StatusServiceUnavailable)
		return false
	default:
	}
	if !accepted {
		writeHTTPError(w, http.StatusUnauthorized)
	}
	return accepted
}

func (s *Server) handleCompanionResponse(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("commandID"), 10, 64)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	s.handleCompanionReport(w, r, id, false)
}

func (s *Server) handleCompanionTimeline(w http.ResponseWriter, r *http.Request) {
	s.handleCompanionReport(w, r, 0, true)
}

func (s *Server) handleCompanionReport(w http.ResponseWriter, r *http.Request, id int64, timeline bool) {
	pr := FromContext(r.Context())
	key, ok := companionPlayerKey(pr)
	if !ok {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	rc := http.NewResponseController(w)
	if rc.SetReadDeadline(time.Now().Add(companionBodyTimeout)) == nil {
		defer rc.SetReadDeadline(time.Time{})
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, companionBodyLimit+1))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if len(body) > companionBodyLimit {
		writeHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/json", "application/xml", "text/xml":
	default:
		ct = model.XML.ContentType()
	}
	if !s.companionRequestCurrent(w, r, s.acceptanceCurrent(r)) {
		return
	}
	reply := companionReply{body: body, contentType: ct}
	c := s.companion
	token := sha256.Sum256([]byte(pr.Token))
	c.mu.Lock()
	p := c.live(key)
	ok = p != nil && p.token == token
	if ok && timeline {
		p.timeline = reply
		p.timeAt = c.now()
		close(p.timeWake)
		p.timeWake = make(chan struct{})
	} else if ok {
		cmd := p.waiting[id]
		if p.subscription != nil && p.subscription.id == id {
			cmd = p.subscription
		}
		ok = cmd != nil && cmd.delivered && !cmd.answered
		if ok {
			cmd.answered = true
			if cmd.reply == nil {
				p.subscription = nil
			} else {
				cmd.reply <- reply
			}
		}
	}
	c.mu.Unlock()
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// relayCompanionCommand queues a controller's command for the target player
// and returns the player's answer.
func (s *Server) relayCompanionCommand(w http.ResponseWriter, r *http.Request, key companionKey) {
	c := s.companion
	c.mu.Lock()
	p := c.live(key)
	if p == nil {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	cmd, ok := c.enqueueLocked(p, strings.Clone(r.URL.Path), r.URL.Query(), true)
	c.mu.Unlock()
	if !ok {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	defer func() {
		c.mu.Lock()
		delete(p.waiting, cmd.id)
		p.dropLocked(cmd)
		c.mu.Unlock()
	}()
	timer := time.NewTimer(c.commandWait)
	defer timer.Stop()
	select {
	case reply, ok := <-cmd.reply:
		if !ok {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if s.companionRequestCurrent(w, r, s.acceptanceCurrent(r)) {
			reply.write(w)
		}
	case <-timer.C:
		if s.companionRequestCurrent(w, r, s.acceptanceCurrent(r)) {
			writeHTTPError(w, http.StatusGatewayTimeout)
		}
	case <-r.Context().Done():
	case <-c.stopping:
		writeHTTPError(w, http.StatusServiceUnavailable)
	case <-s.ctx.Done():
		writeHTTPError(w, http.StatusServiceUnavailable)
	}
}

func (reply companionReply) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", reply.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(reply.body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply.body)
}

// enqueueLocked numbers a command, queues it and wakes the held poll.
func (c *companionRelay) enqueueLocked(p *companionPlayer, path string, q url.Values, wait bool) (*companionCommand, bool) {
	if len(p.queue) >= companionMaxQueued || (wait && len(p.waiting) >= companionMaxQueued) {
		return nil, false
	}
	c.nextID++
	cmd := &companionCommand{id: c.nextID}
	cmd.attrs = append(cmd.attrs, model.Attr{Name: "path", Value: path}, model.Attr{Name: "commandID", Value: strconv.FormatInt(cmd.id, 10)})
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]bool)
	for _, name := range names {
		if !companionArgument(name) || len(q[name]) == 0 ||
			(traceSecretQuery(name) && (name != "token" || path != "/player/playback/playMedia")) {
			continue
		}
		attr := "query" + upperFirst(name)
		if !seen[attr] {
			cmd.attrs = append(cmd.attrs, model.Attr{Name: attr, Value: q[name][0]})
			seen[attr] = true
		}
	}
	if wait {
		cmd.reply = make(chan companionReply, 1)
		p.waiting[cmd.id] = cmd
	}
	p.queue = append(p.queue, cmd)
	select {
	case p.pollWake <- struct{}{}:
	default:
	}
	return cmd, true
}

func (p *companionPlayer) dropLocked(cmd *companionCommand) {
	for i, q := range p.queue {
		if q == cmd {
			copy(p.queue[i:], p.queue[i+1:])
			p.queue[len(p.queue)-1] = nil
			p.queue = p.queue[:len(p.queue)-1]
			return
		}
	}
}

// companionArgument reports whether a controller query argument is handed to
// the player. Transport arguments (credentials, client identity, the
// controller's own command number) stay with the server.
func companionArgument(name string) bool {
	if name == "" || strings.HasPrefix(strings.ToLower(name), "x-plex-") || strings.EqualFold(name, "commandID") {
		return false
	}
	for _, r := range name {
		if !(r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.')) {
			return false
		}
	}
	return true
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// handleCompanionTimelinePoll answers a controller from the player's last
// report. wait=1 holds for a newer report. A player that has not reported
// recently is asked to (timeline/subscribe), as a controller cannot reach it
// any other way.
func (s *Server) handleCompanionTimelinePoll(w http.ResponseWriter, r *http.Request, key companionKey) {
	c := s.companion
	q := r.URL.Query()
	wait := q.Get("wait") == "1" || strings.EqualFold(q.Get("wait"), "true")
	c.mu.Lock()
	p := c.live(key)
	if p == nil {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if wait && p.timeWaiters >= companionTimelineWaiters {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusTooManyRequests)
		return
	}
	now := c.now()
	if now.Sub(p.timeAt) > companionTimelineFresh && now.Sub(p.subscribe) > companionTimelineFresh &&
		(p.subscription == nil || p.subscription.delivered) {
		if cmd, ok := c.enqueueLocked(p, "/player/timeline/subscribe", url.Values{"protocol": {"http"}}, false); ok {
			p.subscribe = now
			p.subscription = cmd
		}
	}
	wake := p.timeWake
	if wait {
		p.timeWaiters++
		defer func() {
			c.mu.Lock()
			p.timeWaiters--
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()

	if wait {
		timer := time.NewTimer(c.pollHold)
		select {
		case <-wake:
		case <-timer.C:
		case <-r.Context().Done():
		case <-c.stopping:
		case <-s.ctx.Done():
		}
		timer.Stop()
	}
	if !s.companionRequestCurrent(w, r, s.acceptanceCurrent(r)) {
		return
	}
	c.mu.Lock()
	reply, live := p.timeline, c.players[key] == p
	c.mu.Unlock()
	if !live {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	// Plex Web checks the receiver identity before applying a timeline.
	w.Header().Set("X-Plex-Client-Identifier", key.clientID)
	if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders+", X-Plex-Client-Identifier")
	}
	if len(reply.body) == 0 {
		s.write(w, r, companionStoppedTimeline(q.Get("commandID")))
		return
	}
	reply.write(w)
}

// handleCompanionClients lists the requesting user's live players.
func (s *Server) handleCompanionClients(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	out := companionClientsContainer{}
	if companionViewer(pr) {
		c := s.companion
		c.mu.Lock()
		c.pruneLocked(c.now())
		for key, p := range c.players {
			if key.userID != pr.User.ID {
				continue
			}
			out.Server = append(out.Server, companionClient{
				Name: p.name, Host: p.address, Address: p.address, MachineIdentifier: key.clientID,
				Version: p.version, Protocol: "plex", Product: p.product, DeviceClass: p.deviceClass,
				ProtocolVersion: p.protocolVersion, ProtocolCapabilities: p.capabilities,
			})
		}
		c.mu.Unlock()
		sort.Slice(out.Server, func(i, j int) bool { return out.Server[i].MachineIdentifier < out.Server[j].MachineIdentifier })
	}
	out.Size = len(out.Server)
	if out.Size == 0 {
		s.writeEmpty(w, r)
		return
	}
	s.write(w, r, &out)
}

type companionPollContainer struct {
	Size    int                       `plex:"size"`
	Command []companionCommandElement `plex:"Command"`
}

type companionCommandElement struct {
	Attrs []model.Attr `plex:",attrs"`
}

type companionClientsContainer struct {
	Size   int               `plex:"size"`
	Server []companionClient `plex:"Server"`
}

type companionClient struct {
	Name                 string `plex:"name"`
	Host                 string `plex:"host"`
	Address              string `plex:"address"`
	Port                 int    `plex:"port"`
	MachineIdentifier    string `plex:"machineIdentifier"`
	Version              string `plex:"version"`
	Protocol             string `plex:"protocol"`
	Product              string `plex:"product"`
	DeviceClass          string `plex:"deviceClass"`
	ProtocolVersion      string `plex:"protocolVersion"`
	ProtocolCapabilities string `plex:"protocolCapabilities"`
}

type companionTimelineContainer struct {
	CommandID string                   `plex:"commandID,omitempty"`
	Location  string                   `plex:"location"`
	Timeline  []companionTimelineState `plex:"Timeline"`
}

type companionTimelineState struct {
	Type  string `plex:"type"`
	State string `plex:"state"`
}

func companionStoppedTimeline(commandID string) *companionTimelineContainer {
	return &companionTimelineContainer{CommandID: commandID, Location: "navigation", Timeline: []companionTimelineState{
		{Type: "video", State: "stopped"}, {Type: "music", State: "stopped"}, {Type: "photo", State: "stopped"},
	}}
}

func companionName(pr *Request) string {
	for _, v := range []string{pr.DeviceName, pr.Device, pr.Product} {
		if v != "" {
			return v
		}
	}
	return pr.ClientID
}
