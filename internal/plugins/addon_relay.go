package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The relay shows an add-on's own configure page inside Ether and catches
// the install link it produces, so an owner never leaves Ether and never
// copies a link by hand.
//
// Safety: the page runs in a sandboxed frame with an opaque origin (no
// cookies, storage or session of the console), every relayed request needs
// a short-lived token that names one add-on origin, and the relay never
// forwards the owner's headers or cookies. Requests leave through the same
// guarded client as every other plugin fetch.

// RelayPublicPrefix is where the console reaches the relay.
const RelayPublicPrefix = "/ether/api/addon-relay/"

const (
	relayTTL      = 30 * time.Minute
	relayMax      = 64
	relayBodyMax  = 1 << 20
	relayPageMax  = 8 << 20
	relayAssetMax = 32 << 20
)

type relaySession struct {
	origin  *url.URL
	site    string // the add-on's site; the page may move between its hosts
	entry   string // escaped path of the configure page
	expires time.Time
}

type addonRelay struct {
	mu       sync.Mutex
	sessions map[string]*relaySession
}

func (m *Manager) relayStore() *addonRelay {
	m.relayOnce.Do(func() { m.relay = &addonRelay{sessions: map[string]*relaySession{}} })
	return m.relay
}

// startRelay opens a relay session for an add-on's configure page and
// returns the console path that shows it.
func (m *Manager) startRelay(l addonLink) (string, error) {
	page := l.withSegment(l.segment)
	page = strings.TrimSuffix(strings.SplitN(page, "?", 2)[0], "manifest.json") + "configure"
	u, err := url.Parse(page)
	if err != nil {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	rs := m.relayStore()
	now := m.now()
	rs.mu.Lock()
	for k, s := range rs.sessions {
		if now.After(s.expires) {
			delete(rs.sessions, k)
		}
	}
	if len(rs.sessions) >= relayMax {
		rs.mu.Unlock()
		return "", errors.New("plugins: too many configure pages are open; close one and try again")
	}
	origin := &url.URL{Scheme: u.Scheme, Host: u.Host}
	rs.sessions[token] = &relaySession{origin: origin, site: siteOf(u.Hostname()), entry: u.EscapedPath(), expires: now.Add(relayTTL)}
	rs.mu.Unlock()
	return RelayPublicPrefix + token + u.EscapedPath(), nil
}

// AddonConfigPage opens the configure page of an installed add-on.
func (m *Manager) AddonConfigPage(ctx context.Context, id string) (string, error) {
	st, err := m.addonState(ctx, id)
	if err != nil {
		return "", err
	}
	return m.startRelay(st.link)
}

// AddonLinkConfigPage opens the configure page for an add-on not yet
// installed, from its install link.
func (m *Manager) AddonLinkConfigPage(link string) (string, error) {
	link = strings.TrimSpace(link)
	if strings.HasPrefix(strings.ToLower(link), "stremio://") {
		link = "https://" + link[len("stremio://"):]
	}
	l, err := parseAddonLink(link)
	if err != nil {
		return "", &ValidationError{[]FieldError{{"url", "That is not an add-on install link."}}}
	}
	return m.startRelay(l)
}

// follow moves a session to the host its page settled on, so the page's
// relative requests reach that host.
func (rs *addonRelay) follow(token string, final *url.URL) *url.URL {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	s := rs.sessions[token]
	if s == nil {
		return &url.URL{Scheme: final.Scheme, Host: final.Host}
	}
	if final.Host != s.origin.Host {
		s.origin = &url.URL{Scheme: final.Scheme, Host: final.Host}
	}
	return s.origin
}

// siteOf is a host's last two labels: a crude registrable domain that keeps
// sister hosts of one add-on together (peerflix.mov, config.peerflix.mov).
func siteOf(h string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSuffix(h, ".")), ".")
	if len(parts) <= 2 {
		return strings.Join(parts, ".")
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// relayHost is the form a sister host takes in a relay path:
// {prefix}/~/{host}/{path}.
var relayHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+(?::[0-9]{1,5})?$`)

func (rs *addonRelay) session(token string, now time.Time) *relaySession {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	s := rs.sessions[token]
	if s == nil || now.After(s.expires) {
		return nil
	}
	return s
}

var (
	relayAttr = regexp.MustCompile(`(?i)(\s(?:src|href|action|poster|data-src)\s*=\s*["'])/([^/"'])`)
	relayCSS  = regexp.MustCompile(`(?i)(url\(\s*["']?)/([^/"')])`)
	relayHead = regexp.MustCompile(`(?i)<head[^>]*>`)
)

// RelayHandler serves /api/addon-relay/{token}/{path...} without the
// console's sign-in: the token is the permission.
func (m *Manager) RelayHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Accept")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Del("X-Frame-Options")
		h.Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals; frame-ancestors 'self'")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		rest := strings.TrimPrefix(r.URL.EscapedPath(), "/")
		token, path, _ := strings.Cut(rest, "/")
		s := m.relayStore().session(token, m.now())
		if s == nil {
			http.Error(w, "This settings page has expired. Open it again from Ether.", http.StatusGone)
			return
		}
		if path == "" {
			path = strings.TrimPrefix(s.entry, "/")
		}
		host := s.origin.Host
		if rest, ok := strings.CutPrefix(path, "~/"); ok {
			// A sister host of the add-on, reached by a link or redirect
			// the page itself pointed at.
			h, p, _ := strings.Cut(rest, "/")
			h = strings.ToLower(h)
			if !relayHost.MatchString(h) || siteOf(strings.Split(h, ":")[0]) != s.site {
				http.Error(w, "That page belongs to another site.", http.StatusForbidden)
				return
			}
			host, path = h, p
		}
		target := s.origin.Scheme + "://" + host + "/" + path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		var body io.Reader
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			b, err := io.ReadAll(io.LimitReader(r.Body, relayBodyMax+1))
			if err != nil || len(b) > relayBodyMax {
				http.Error(w, "Request too large.", http.StatusRequestEntityTooLarge)
				return
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
		if err != nil {
			http.Error(w, "Bad request.", http.StatusBadRequest)
			return
		}
		for _, k := range []string{"Content-Type", "Accept", "Accept-Language"} {
			if v := r.Header.Get(k); v != "" {
				req.Header.Set(k, v)
			}
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Ether add-on settings)")
		client := *m.client
		// Follow redirects here so the frame never leaves the relay. A page
		// may move to a sister host of the same add-on (peerflix.mov to
		// addon.peerflix.mov); anywhere else is refused.
		client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if next.URL.Scheme != "https" && next.URL.Scheme != s.origin.Scheme {
				return errors.New("insecure redirect")
			}
			if siteOf(next.URL.Hostname()) != s.site {
				return errors.New("redirect to another site")
			}
			return nil
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "The add-on's settings page didn't answer.", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		final := resp.Request.URL
		ct := resp.Header.Get("Content-Type")
		lower := strings.ToLower(ct)
		// A page (not an asset) decides which host the page's own
		// root-relative requests go to.
		origin := s.origin
		if strings.Contains(lower, "text/html") {
			origin = m.relayStore().follow(token, final)
		}
		prefix := RelayPublicPrefix + token
		originStr := origin.Scheme + "://" + origin.Host
		docPath := final.EscapedPath()
		if docPath == "" {
			docPath = "/"
		}
		if ct != "" {
			h.Set("Content-Type", ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			h.Set("Cache-Control", cc)
		}
		switch {
		case strings.Contains(lower, "text/html"):
			page, err := io.ReadAll(io.LimitReader(resp.Body, relayPageMax))
			if err != nil {
				http.Error(w, "The page could not be read.", http.StatusBadGateway)
				return
			}
			page = relayRewrite(page, prefix, originStr, s.site)
			page = injectWatcher(page, prefix, originStr, docPath, s.site)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(page)
		case strings.Contains(lower, "text/css"):
			css, err := io.ReadAll(io.LimitReader(resp.Body, relayPageMax))
			if err != nil {
				http.Error(w, "The page could not be read.", http.StatusBadGateway)
				return
			}
			css = relayCSS.ReplaceAll(css, []byte("${1}"+prefix+"/${2}"))
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(css)
		default:
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(resp.Body, relayAssetMax))
		}
	})
}

// relaySister finds absolute links to a host in an attribute or a meta
// refresh (content="0; url=https://config.example.com").
var relaySister = regexp.MustCompile(`(?i)((?:=\s*["']|url=)\s*)https?://([a-z0-9.-]+(?::[0-9]{1,5})?)(/|["'\s;>]|$)`)

func relayRewrite(page []byte, prefix, origin, site string) []byte {
	page = bytes.ReplaceAll(page, []byte(`="`+origin+`/`), []byte(`="`+prefix+`/`))
	page = bytes.ReplaceAll(page, []byte(`='`+origin+`/`), []byte(`='`+prefix+`/`))
	page = relaySister.ReplaceAllFunc(page, func(m []byte) []byte {
		g := relaySister.FindSubmatch(m)
		host := strings.ToLower(string(g[2]))
		if siteOf(strings.Split(host, ":")[0]) != site {
			return m
		}
		tail := string(g[3])
		if tail != "/" {
			tail = "/" + tail
		}
		return []byte(string(g[1]) + prefix + "/~/" + host + tail)
	})
	page = relayAttr.ReplaceAll(page, []byte("${1}"+prefix+"/${2}"))
	return relayCSS.ReplaceAll(page, []byte("${1}"+prefix+"/${2}"))
}

// watcherJS runs first in the relayed page. It keeps the page's own
// requests on the relay, gives a sandboxed page working storage, and tells
// Ether about install links: clicked, opened or copied ones as "link"
// (Ether applies them), ones only shown on the page as "candidate".
const watcherJS = `(function(){var P=%s,O=%s,D=%s,S=%s;
try{history.replaceState(history.state,"",D+location.search+location.hash)}catch(e){}
var H=location.protocol+"//"+location.host;
function site(h){return h.toLowerCase().split(":")[0].split(".").slice(-2).join(".")}
function fix(u){if(typeof URL!=="undefined"&&u instanceof URL)u=u.href;if(typeof u!=="string")return u;if(u.indexOf("//")===0)u=location.protocol+u;if(u.indexOf(H+"/")===0&&u.indexOf(H+P+"/")!==0)u=u.slice(H.length);if(u.charAt(0)==="/")return P+u;if(u.indexOf(O)===0)return P+u.slice(O.length);var m=/^https?:\/\/([^\/?#]+)(.*)$/i.exec(u);if(m&&site(m[1])===S)return P+"/~/"+m[1].toLowerCase()+(m[2].charAt(0)==="/"?m[2]:"/"+m[2]);return u}
try{if(window.navigation)navigation.addEventListener("navigate",function(e){var u=e.destination&&e.destination.url;if(!u||!e.cancelable||u.indexOf(H)===0)return;var r=fix(u);if(r!==u){e.preventDefault();location.href=r}})}catch(e){}
var f=window.fetch;if(f)window.fetch=function(i,o){try{if(typeof i==="string")i=fix(i);else if(i instanceof URL)i=fix(i.href);else if(i&&i.url){var r=fix(i.url);if(r!==i.url)i=new Request(r,i)}}catch(e){}return f.call(this,i,o)};
var sa=Element.prototype.setAttribute;Element.prototype.setAttribute=function(n,v){if((n==="src"||n==="href"||n==="action"||n==="poster")&&typeof v==="string"&&this.tagName!=="A"&&this.tagName!=="FORM")v=fix(v);return sa.call(this,n,v)};
[[window.HTMLImageElement,"src"],[window.HTMLScriptElement,"src"],[window.HTMLLinkElement,"href"],[window.HTMLSourceElement,"src"],[window.HTMLIFrameElement,"src"],[window.HTMLVideoElement,"poster"]].forEach(function(p){try{var d=Object.getOwnPropertyDescriptor(p[0].prototype,p[1]);if(d&&d.set)Object.defineProperty(p[0].prototype,p[1],{configurable:true,enumerable:d.enumerable,get:d.get,set:function(v){d.set.call(this,typeof v==="string"?fix(v):v)}})}catch(e){}});
var xo=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){var a=[].slice.call(arguments);a[1]=fix(u);return xo.apply(this,a)};
function mem(){var d={};return{getItem:function(k){return k in d?d[k]:null},setItem:function(k,v){d[k]=String(v)},removeItem:function(k){delete d[k]},clear:function(){d={}},key:function(i){return Object.keys(d)[i]||null},get length(){return Object.keys(d).length}}}
["localStorage","sessionStorage"].forEach(function(n){try{window[n].length}catch(e){try{Object.defineProperty(window,n,{value:mem(),configurable:true})}catch(e2){}}});
try{document.cookie}catch(e){try{Object.defineProperty(Document.prototype,"cookie",{get:function(){return""},set:function(){},configurable:true})}catch(e2){}}
var LINK=/(stremio:\/\/[^\s"'<>]+?\/manifest\.json|https?:\/\/[^\s"'<>]+?\/manifest\.json)/i;
function norm(u){u=String(u||"");var m=LINK.exec(u);if(!m)return"";u=m[1];if(/^stremio:\/\//i.test(u))u="https://"+u.slice(10);if(u.indexOf(H+P)===0)u=u.slice(H.length);else if(u.indexOf(H+"/")===0)return O+u.slice(H.length);if(u.indexOf(P+"/~/")===0)return"https://"+u.slice(P.length+3);if(u.indexOf(P)===0)u=O+u.slice(P.length);return u}
var sent="";function report(t,u){u=norm(u);if(!u||(t==="link"&&u===sent))return false;if(t==="link")sent=u;parent.postMessage({source:"ether-addon-relay",type:t,url:u},"*");return true}
document.addEventListener("click",function(e){var a=e.target&&e.target.closest&&e.target.closest("a[href]");if(a&&report("link",a.getAttribute("href"))){e.preventDefault();e.stopPropagation()}},true);
var hc=HTMLElement.prototype.click;HTMLElement.prototype.click=function(){if(this.tagName==="A"&&report("link",this.getAttribute("href")))return;return hc.apply(this,arguments)};
var wo=window.open;window.open=function(u){if(report("link",u))return null;return wo.apply(this,arguments)};
try{if(navigator.clipboard&&navigator.clipboard.writeText){var cw=navigator.clipboard.writeText.bind(navigator.clipboard);navigator.clipboard.writeText=function(t){report("link",t);try{return cw(t).catch(function(){})}catch(e){return Promise.resolve()}}}}catch(e){}
document.addEventListener("copy",function(){try{report("link",String(document.getSelection()))}catch(e){}},true);
function shown(el){try{if((el.tagName==="INPUT"||el.tagName==="TEXTAREA")&&el.value.indexOf(H+"/")>=0)el.value=el.value.split(H+"/").join(O+"/");else if(!el.children.length&&el.textContent.indexOf(H+"/")>=0)el.textContent=el.textContent.split(H+"/").join(O+"/")}catch(e){}}
var last="";function scan(){var sh=document.querySelectorAll("input,textarea,code,pre,span,div,p");for(var j=0;j<sh.length;j++)shown(sh[j]);var els=document.querySelectorAll("input,textarea,a[href],code,pre");for(var i=0;i<els.length;i++){var el=els[i];var v=el.value||(el.getAttribute&&el.getAttribute("href"))||el.textContent||"";var u=norm(v);if(u&&u!==last){last=u;report("candidate",u);return}}}
var t=0;new MutationObserver(function(){clearTimeout(t);t=setTimeout(scan,250)}).observe(document.documentElement,{subtree:true,childList:true,attributes:true,characterData:true});
document.addEventListener("input",function(){clearTimeout(t);t=setTimeout(scan,250)},true);
})();`

// injectWatcher adds the watcher and a base for relative URLs. The page
// sees its own path (single-page apps route by it) while every relative
// and root-relative request still goes through the relay.
func injectWatcher(page []byte, prefix, origin, docPath, site string) []byte {
	p, _ := json.Marshal(prefix)
	o, _ := json.Marshal(origin)
	docPath = strings.SplitN(docPath, "?", 2)[0]
	d, _ := json.Marshal(docPath)
	st, _ := json.Marshal(site)
	js := strings.Replace(watcherJS, "%s", string(p), 1)
	js = strings.Replace(js, "%s", string(o), 1)
	js = strings.Replace(js, "%s", string(d), 1)
	js = strings.Replace(js, "%s", string(st), 1)
	dir := docPath[:strings.LastIndex(docPath, "/")+1]
	head := []byte(`<base href="` + prefix + dir + `"><script>` + js + "</script>")
	if loc := relayHead.FindIndex(page); loc != nil {
		out := make([]byte, 0, len(page)+len(head))
		out = append(out, page[:loc[1]]...)
		out = append(out, head...)
		return append(out, page[loc[1]:]...)
	}
	return append(head, page...)
}

/* ------------------------------------------------------------------- HTTP */

func (m *Manager) handleAddonConfigPage(w http.ResponseWriter, r *http.Request) {
	entry, err := m.AddonConfigPage(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"page": entry})
}

func (m *Manager) handleAddonLinkConfigPage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	entry, err := m.AddonLinkConfigPage(body.URL)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"page": entry})
}
