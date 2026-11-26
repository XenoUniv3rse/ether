package plex

import (
	"io"
	"net/http"
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
// A command reaches only a player polled by the same Ether user as the
// controller. A controller's playMedia carries a token argument, so a player
// entry is keyed by client identifier and user: another user polling with the
// same identifier gets an entry of its own.

const (
	companionPollHold    = 20 * time.Second
	companionCommandWait = 15 * time.Second
	companionStale       = 60 * time.Second
	companionBodyLimit   = 64 << 10
	companionMaxPlayers  = 256
	companionMaxQueued   = 16
	// A timeline older than this means the player is not reporting; a
	// controller's timeline poll then asks it to (timeline/subscribe).
	companionTimelineFresh = 10 * time.Second
)

type companionKey struct {
	clientID string
	userID   int64
}

type companionCommand struct {
	id    int64
	attrs []model.Attr
	reply chan companionReply // nil when nobody waits for the answer
}

type companionReply struct {
	body        []byte
	contentType string
}

type companionPlayer struct {
	key companionKey

	name, product, version, platform, platformVersion, device string
	address                                                   string
	deviceClass, protocolVersion, capabilities                string

	lastPoll  time.Time
	queue     []*companionCommand
	pollWake  chan struct{} // closed to release the held poll
	nextID    int64
	waiting   map[int64]chan companionReply
	timeline  []byte
	timeType  string
	timeAt    time.Time
	timeWake  chan struct{} // closed when a new timeline arrives
	subscribe time.Time     // last timeline/subscribe the relay queued itself
}

type companionRelay struct {
	mu          sync.Mutex
	players     map[companionKey]*companionPlayer
	pollHold    time.Duration
	commandWait time.Duration
	stale       time.Duration
	now         func() time.Time
}

func newCompanionRelay() *companionRelay {
	return &companionRelay{
		players:     map[companionKey]*companionPlayer{},
		pollHold:    companionPollHold,
		commandWait: companionCommandWait,
		stale:       companionStale,
		now:         time.Now,
	}
}

// pruneLocked drops players that stopped polling. A player with a held poll
// is never stale: its lastPoll is refreshed when the poll ends.
func (c *companionRelay) pruneLocked(now time.Time) {
	for k, p := range c.players {
		if now.Sub(p.lastPoll) > c.stale+c.pollHold {
			for _, ch := range p.waiting {
				close(ch)
			}
			delete(c.players, k)
		}
	}
}

func (c *companionRelay) live(key companionKey) *companionPlayer {
	now := c.now()
	c.pruneLocked(now)
	return c.players[key]
}

func companionPlayerKey(pr *Request) (companionKey, bool) {
	if pr.User == nil || pr.Capability || pr.ClientID == "" {
		return companionKey{}, false
	}
	return companionKey{clientID: pr.ClientID, userID: pr.User.ID}, true
}

// handleCompanion serves every /player/* path.
func (s *Server) handleCompanion(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/player/proxy/poll":
		s.handleCompanionPoll(w, r)
		return
	case "/player/proxy/response":
		s.handleCompanionResponse(w, r)
		return
	case "/player/proxy/timeline":
		s.handleCompanionTimeline(w, r)
		return
	}
	target := plexParam(r, "X-Plex-Target-Client-Identifier")
	pr := FromContext(r.Context())
	if target == "" || pr.User == nil || pr.Capability || strings.HasPrefix(r.URL.Path, "/player/proxy/") {
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

func (s *Server) handleCompanionPoll(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	key, ok := companionPlayerKey(pr)
	if !ok {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	c := s.companion
	q := r.URL.Query()
	c.mu.Lock()
	now := c.now()
	c.pruneLocked(now)
	p := c.players[key]
	if p == nil {
		if len(c.players) >= companionMaxPlayers {
			c.mu.Unlock()
			writeHTTPError(w, http.StatusServiceUnavailable)
			return
		}
		p = &companionPlayer{key: key, waiting: map[int64]chan companionReply{}, timeWake: make(chan struct{})}
		c.players[key] = p
	}
	p.name = companionName(pr)
	p.product, p.version, p.platform, p.platformVersion, p.device = pr.Product, pr.Version, pr.Platform, pr.PlatformVersion, pr.Device
	if pr.RemoteIP.IsValid() {
		p.address = pr.RemoteIP.String()
	}
	p.deviceClass = q.Get("deviceClass")
	p.protocolVersion = q.Get("protocolVersion")
	p.capabilities = q.Get("protocolCapabilities")
	p.lastPoll = now
	// One held poll per player: a newer poll releases the older one empty.
	if p.pollWake != nil {
		close(p.pollWake)
	}
	wake := make(chan struct{})
	p.pollWake = wake
	cmd := p.popLocked()
	c.mu.Unlock()

	if cmd == nil {
		timer := time.NewTimer(c.pollHold)
		select {
		case <-wake:
		case <-timer.C:
		case <-r.Context().Done():
		case <-s.ctx.Done():
		}
		timer.Stop()
		c.mu.Lock()
		// nil: a command released this poll. Another channel: a newer poll
		// superseded this one and takes the queue.
		if p.pollWake == wake || p.pollWake == nil {
			p.pollWake = nil
			cmd = p.popLocked()
		}
		p.lastPoll = c.now()
		c.mu.Unlock()
		if r.Context().Err() != nil {
			if cmd != nil {
				c.requeue(p, cmd)
			}
			return
		}
	}
	out := companionPollContainer{}
	if cmd != nil {
		out.Size = 1
		out.Command = []companionCommandElement{{Attrs: cmd.attrs}}
	}
	s.write(w, r, &out)
}

func (p *companionPlayer) popLocked() *companionCommand {
	if len(p.queue) == 0 {
		return nil
	}
	cmd := p.queue[0]
	p.queue = p.queue[1:]
	return cmd
}

func (c *companionRelay) requeue(p *companionPlayer, cmd *companionCommand) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.queue = append([]*companionCommand{cmd}, p.queue...)
}

func (s *Server) handleCompanionResponse(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	key, ok := companionPlayerKey(pr)
	if !ok {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("commandID"), 10, 64)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, companionBodyLimit+1))
	if err != nil || len(body) > companionBodyLimit {
		writeHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	c := s.companion
	c.mu.Lock()
	var ch chan companionReply
	if p := c.live(key); p != nil {
		ch = p.waiting[id]
		delete(p.waiting, id)
	}
	c.mu.Unlock()
	if ch == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	ch <- companionReply{body: body, contentType: r.Header.Get("Content-Type")}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCompanionTimeline(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	key, ok := companionPlayerKey(pr)
	if !ok {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, companionBodyLimit+1))
	if err != nil || len(body) > companionBodyLimit {
		writeHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	c := s.companion
	c.mu.Lock()
	p := c.live(key)
	if p == nil {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	p.timeline = body
	p.timeType = r.Header.Get("Content-Type")
	p.timeAt = c.now()
	close(p.timeWake)
	p.timeWake = make(chan struct{})
	c.mu.Unlock()
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
	cmd, ok := p.enqueueLocked(r.URL.Path, r.URL.Query(), true)
	c.mu.Unlock()
	if !ok {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	timer := time.NewTimer(c.commandWait)
	defer timer.Stop()
	select {
	case reply, ok := <-cmd.reply:
		if !ok {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		ct := reply.contentType
		if ct == "" {
			ct = model.XML.ContentType()
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.Itoa(len(reply.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(reply.body)
		return
	case <-timer.C:
	case <-r.Context().Done():
	case <-s.ctx.Done():
	}
	c.mu.Lock()
	delete(p.waiting, cmd.id)
	p.dropLocked(cmd)
	c.mu.Unlock()
	if r.Context().Err() == nil {
		writeHTTPError(w, http.StatusGatewayTimeout)
	}
}

// enqueueLocked numbers a command, queues it and wakes the held poll.
func (p *companionPlayer) enqueueLocked(path string, q map[string][]string, wait bool) (*companionCommand, bool) {
	if len(p.queue) >= companionMaxQueued {
		return nil, false
	}
	p.nextID++
	cmd := &companionCommand{id: p.nextID}
	cmd.attrs = append(cmd.attrs, model.Attr{Name: "path", Value: path}, model.Attr{Name: "commandID", Value: strconv.FormatInt(cmd.id, 10)})
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !companionArgument(name) || len(q[name]) == 0 {
			continue
		}
		cmd.attrs = append(cmd.attrs, model.Attr{Name: "query" + upperFirst(name), Value: q[name][0]})
	}
	if wait {
		cmd.reply = make(chan companionReply, 1)
		p.waiting[cmd.id] = cmd.reply
	}
	p.queue = append(p.queue, cmd)
	if p.pollWake != nil {
		close(p.pollWake)
		p.pollWake = nil
	}
	return cmd, true
}

func (p *companionPlayer) dropLocked(cmd *companionCommand) {
	for i, q := range p.queue {
		if q == cmd {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
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
	c.mu.Lock()
	p := c.live(key)
	if p == nil {
		c.mu.Unlock()
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	now := c.now()
	if now.Sub(p.timeAt) > companionTimelineFresh && now.Sub(p.subscribe) > companionTimelineFresh {
		p.subscribe = now
		p.enqueueLocked("/player/timeline/subscribe", map[string][]string{"protocol": {"http"}}, false)
	}
	wake := p.timeWake
	c.mu.Unlock()

	if v := r.URL.Query().Get("wait"); v == "1" || strings.EqualFold(v, "true") {
		timer := time.NewTimer(c.pollHold)
		select {
		case <-wake:
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-s.ctx.Done():
		}
		timer.Stop()
	}
	c.mu.Lock()
	body, ct := p.timeline, p.timeType
	c.mu.Unlock()
	if len(body) == 0 {
		s.write(w, r, companionStoppedTimeline(r.URL.Query().Get("commandID")))
		return
	}
	if ct == "" {
		ct = model.XML.ContentType()
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleCompanionClients lists the requesting user's live players.
func (s *Server) handleCompanionClients(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	out := companionClientsContainer{}
	if pr.User != nil && !pr.Capability {
		c := s.companion
		c.mu.Lock()
		c.pruneLocked(c.now())
		for _, p := range c.players {
			if p.key.userID != pr.User.ID {
				continue
			}
			out.Server = append(out.Server, companionClient{
				Name: p.name, Host: p.address, Address: p.address, MachineIdentifier: p.key.clientID,
				Version: p.version, Protocol: "plex", Product: p.product, DeviceClass: p.deviceClass,
				ProtocolVersion: p.protocolVersion, ProtocolCapabilities: p.capabilities,
			})
		}
		c.mu.Unlock()
		sort.Slice(out.Server, func(i, j int) bool { return out.Server[i].MachineIdentifier < out.Server[j].MachineIdentifier })
	}
	out.Size = len(out.Server)
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
