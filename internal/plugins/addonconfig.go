package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/braedonsaunders/ether/internal/stremio"
)

// A Stremio add-on keeps its settings in its own install link: the path
// segment before /manifest.json. Each add-on picks its own way of writing
// that segment, and none of the popular ones publishes a schema, so Ether
// reads it generically instead of knowing any add-on:
//
//   - declared: the add-on lists its settings in manifest.config (the
//     Stremio SDK convention); Ether draws them and writes the SDK's JSON.
//   - decoded: the segment is readable JSON, base64 JSON or key=value pairs;
//     Ether turns the values into typed fields and writes them back in the
//     same shape, so the add-on reads exactly what it would have written.
//   - page: the segment is opaque (encrypted by the add-on); the owner uses
//     the add-on's own configure page inside Ether (see addon_relay.go).
//
// Secrets found in any of these are never sent to the console.

// AddonConfigField is one setting as the console draws it.
type AddonConfigField struct {
	Key      string              `json:"key"`
	Label    string              `json:"label"`
	Kind     string              `json:"kind"` // text, number, switch, list, secret, select, fixed
	Value    any                 `json:"value,omitempty"`
	Set      bool                `json:"set,omitempty"` // a secret has a value (never sent)
	Options  []AddonConfigOption `json:"options,omitempty"`
	Required bool                `json:"required,omitempty"`
	Summary  string              `json:"summary,omitempty"` // fixed (structured) values, described
	// Group names the nested setting a field belongs to ("Languages"); its
	// Key is then a path ("languages/preferred").
	Group string `json:"group,omitempty"`
}

// AddonConfigOption is one choice of a select field.
type AddonConfigOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// AddonConfig describes how an add-on is configured, for the console.
type AddonConfig struct {
	Configurable bool               `json:"configurable"`
	Required     bool               `json:"required"`
	Configured   bool               `json:"configured"`
	Layer        string             `json:"layer"` // declared, decoded, page, none
	Fields       []AddonConfigField `json:"fields"`
	Host         string             `json:"host"`
}

// ErrNoAddonLink reports a plugin without a sealed add-on link.
var ErrNoAddonLink = errors.New("plugins: this plugin has no add-on link")

/* --------------------------------------------------------------- the link */

// addonLink splits an install link into the add-on's root and its
// configuration segment (escaped, as written in the link).
type addonLink struct {
	root    *url.URL // scheme://host/prefix (no trailing slash)
	segment string   // escaped config segment, "" when unconfigured
	query   string
}

func parseAddonLink(raw string) (addonLink, error) {
	// Ether stores add-on links normalised (no /manifest.json); owners and
	// add-on pages give them whole, or as stremio:// links. Read them all.
	u, err := stremio.NormalizeURL(raw)
	if err != nil {
		return addonLink{}, errors.New("plugins: not an add-on link")
	}
	dir := strings.TrimSuffix(u.EscapedPath(), "/")
	root := *u
	root.RawQuery, root.Fragment = "", ""
	out := addonLink{query: u.RawQuery}
	if i := strings.LastIndex(dir, "/"); i >= 0 && dir[i+1:] != "" {
		out.segment = dir[i+1:]
		setPath(&root, dir[:i])
	} else {
		setPath(&root, dir)
	}
	out.root = &root
	return out, nil
}

func setPath(u *url.URL, escaped string) {
	u.RawPath = escaped
	if p, err := url.PathUnescape(escaped); err == nil {
		u.Path = p
	} else {
		u.Path = escaped
	}
	if u.Path == u.RawPath {
		u.RawPath = ""
	}
}

// base is the root plus prefix as a string, without a trailing slash.
func (l addonLink) base() string {
	return strings.TrimSuffix(l.root.String(), "/")
}

// withSegment is the install link for a configuration segment ("" for none).
func (l addonLink) withSegment(seg string) string {
	s := l.base()
	if seg != "" {
		s += "/" + seg
	}
	s += "/manifest.json"
	if l.query != "" {
		s += "?" + l.query
	}
	return s
}

// unconfigured is the same link with its last segment as part of the root:
// for add-ons hosted under a path prefix the last segment is not settings.
func (l addonLink) asRoot() addonLink {
	if l.segment == "" {
		return l
	}
	root := *l.root
	setPath(&root, strings.TrimSuffix(l.root.EscapedPath(), "/")+"/"+l.segment)
	return addonLink{root: &root, query: l.query}
}

/* ------------------------------------------------------------- the values */

// cfgValue is one setting with its original JSON shape, in order.
type cfgValue struct {
	key string
	raw json.RawMessage // JSON value as read (strings quoted)
}

// cfgDoc is a decoded configuration and how to write it back.
type cfgDoc struct {
	format string // json, b64json, kv
	// b64json: which alphabet and padding the add-on used.
	b64url, b64pad bool
	// kv: the pair separator and whether the add-on escaped the segment.
	sep     string
	escaped bool
	values  []cfgValue
}

var kvKey = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

// decodeSegment reads a configuration segment in any shape Ether knows.
func decodeSegment(seg string) (cfgDoc, bool) {
	if seg == "" || len(seg) > 16<<10 {
		return cfgDoc{}, false
	}
	plain, err := url.PathUnescape(seg)
	if err != nil {
		plain = seg
	}
	escaped := plain != seg
	if vals, ok := orderedObject([]byte(plain)); ok {
		return cfgDoc{format: "json", escaped: escaped, values: vals}, true
	}
	for _, enc := range []struct {
		url, pad bool
		e        *base64.Encoding
	}{{false, true, base64.StdEncoding}, {false, false, base64.RawStdEncoding}, {true, true, base64.URLEncoding}, {true, false, base64.RawURLEncoding}} {
		if b, err := enc.e.DecodeString(plain); err == nil {
			if vals, ok := orderedObject(b); ok {
				return cfgDoc{format: "b64json", b64url: enc.url, b64pad: enc.pad, escaped: escaped, values: vals}, true
			}
		}
	}
	for _, sep := range []string{"|", "&", ";"} {
		if vals, ok := keyValues(plain, sep); ok {
			return cfgDoc{format: "kv", sep: sep, escaped: escaped, values: vals}, true
		}
	}
	return cfgDoc{}, false
}

// orderedObject reads a JSON object keeping its key order.
func orderedObject(b []byte) ([]cfgValue, bool) {
	b = bytes.TrimSpace(b)
	if len(b) < 2 || b[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var out []cfgValue
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		k, ok := t.(string)
		if !ok {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		out = append(out, cfgValue{key: k, raw: raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	return out, true
}

// keyValues reads key=value pairs joined by sep (Torrentio's shape).
func keyValues(s, sep string) ([]cfgValue, bool) {
	if !strings.Contains(s, "=") {
		return nil, false
	}
	var out []cfgValue
	for _, part := range strings.Split(s, sep) {
		k, v, ok := strings.Cut(part, "=")
		if !ok || !kvKey.MatchString(k) {
			return nil, false
		}
		raw, _ := json.Marshal(v)
		out = append(out, cfgValue{key: k, raw: raw})
	}
	return out, len(out) > 0
}

// encode writes the document back in the add-on's own shape.
func (d cfgDoc) encode() (string, error) {
	switch d.format {
	case "json", "b64json":
		buf := objectBytes(d.values)
		if d.format == "json" {
			return url.PathEscape(string(buf)), nil
		}
		enc := base64.StdEncoding
		switch {
		case d.b64url && d.b64pad:
			enc = base64.URLEncoding
		case d.b64url:
			enc = base64.RawURLEncoding
		case !d.b64pad:
			enc = base64.RawStdEncoding
		}
		s := enc.EncodeToString(buf)
		if d.escaped || !d.b64url {
			// Standard base64 carries "/" and "+"; a path needs them escaped.
			s = url.PathEscape(s)
		}
		return s, nil
	case "kv":
		parts := make([]string, 0, len(d.values))
		for _, v := range d.values {
			var s string
			if json.Unmarshal(v.raw, &s) != nil {
				s = strings.Trim(string(v.raw), `"`)
			}
			parts = append(parts, v.key+"="+s)
		}
		s := strings.Join(parts, d.sep)
		if d.escaped {
			return url.PathEscape(s), nil
		}
		return strings.NewReplacer("/", "%2F", "?", "%3F", "#", "%23", " ", "%20").Replace(s), nil
	}
	return "", fmt.Errorf("plugins: unknown configuration format %q", d.format)
}

/* ------------------------------------------------------------- the fields */

var secretKey = regexp.MustCompile(`(?i)(key|token|secret|passw|pass$|auth|cookie|session|credential|apikey|api_key)`)
var tokenLike = regexp.MustCompile(`^[A-Za-z0-9_\-.:]{20,}$`)

// looksSecret flags credentials by name, or by a long random-looking value.
func looksSecret(key, value string) bool {
	if secretKey.MatchString(key) {
		return true
	}
	if !tokenLike.MatchString(value) {
		return false
	}
	var letters, digits bool
	for _, r := range value {
		letters = letters || unicode.IsLetter(r)
		digits = digits || unicode.IsDigit(r)
	}
	return letters && digits
}

// humanLabel turns "debridOptions" or "debrid_options" into "Debrid options".
func humanLabel(key string) string {
	var words []string
	cur := []rune{}
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(key)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]))):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	if len(words) == 0 {
		return key
	}
	s := strings.Join(words, " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// cfgNest is how deep nested settings become fields of their own
// ({"languages":{"preferred":["en"]}} is Languages › Preferred).
const cfgNest = 2

// cfgPath joins a nested key onto its parent's path, JSON Pointer style.
func cfgPath(parent, key string) string {
	key = strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
	if parent == "" {
		return key
	}
	return parent + "/" + key
}

// fieldsOf types each decoded value for the console. Values of secrets
// are dropped; the console only learns that they are set.
func fieldsOf(d cfgDoc) []AddonConfigField {
	fields := fieldsAt(d, d.values, "", "", 0)
	// Plain settings first, then each nested group under its heading.
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].Group == "" && fields[j].Group != "" })
	return fields
}

func fieldsAt(d cfgDoc, vals []cfgValue, parent, group string, depth int) []AddonConfigField {
	out := make([]AddonConfigField, 0, len(vals))
	for _, v := range vals {
		if depth < cfgNest {
			if sub, ok := orderedObject(v.raw); ok && len(sub) > 0 {
				g := humanLabel(v.key)
				if group != "" {
					g = group + " › " + g
				}
				out = append(out, fieldsAt(d, sub, cfgPath(parent, v.key), g, depth+1)...)
				continue
			}
		}
		f := AddonConfigField{Key: cfgPath(parent, v.key), Label: humanLabel(v.key), Group: group}
		var s string
		var n json.Number
		var b bool
		var list []string
		switch {
		case json.Unmarshal(v.raw, &b) == nil:
			f.Kind, f.Value = "switch", b
		case json.Unmarshal(v.raw, &n) == nil && !bytes.HasPrefix(v.raw, []byte(`"`)):
			f.Kind, f.Value = "number", n
		case json.Unmarshal(v.raw, &s) == nil:
			switch {
			case looksSecret(v.key, s):
				f.Kind, f.Set = "secret", s != ""
			case d.format == "kv" && (s == "true" || s == "false"):
				f.Kind, f.Value = "switch", s == "true"
			case d.format == "kv" && strings.Contains(s, ","):
				f.Kind, f.Value = "list", strings.Split(s, ",")
			default:
				f.Kind, f.Value = "text", s
			}
		case json.Unmarshal(v.raw, &list) == nil:
			f.Kind, f.Value = "list", list
		default:
			// Nested settings stay exactly as the add-on wrote them.
			f.Kind, f.Summary = "fixed", describeRaw(v.raw)
		}
		out = append(out, f)
	}
	return out
}

func describeRaw(raw json.RawMessage) string {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return fmt.Sprintf("%d items", len(arr))
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, humanLabel(k))
		}
		sort.Strings(keys)
		return strings.Join(keys, ", ")
	}
	return "set"
}

// apply writes the console's values into the document. A secret left
// empty keeps its saved value; fixed values are never changed here.
func (d cfgDoc) apply(fields []AddonConfigField, values map[string]any) (cfgDoc, error) {
	kinds := map[string]string{}
	for _, f := range fields {
		kinds[f.Key] = f.Kind
	}
	out := d
	vals, err := d.applyAt(d.values, "", kinds, values)
	if err != nil {
		return cfgDoc{}, err
	}
	out.values = vals
	return out, nil
}

func (d cfgDoc) applyAt(in []cfgValue, parent string, kinds map[string]string, values map[string]any) ([]cfgValue, error) {
	out := make([]cfgValue, len(in))
	copy(out, in)
	for i, v := range out {
		path := cfgPath(parent, v.key)
		if _, typed := kinds[path]; !typed {
			// A nested object whose settings were offered one by one.
			if sub, ok := orderedObject(v.raw); ok && len(sub) > 0 {
				next, err := d.applyAt(sub, path, kinds, values)
				if err != nil {
					return nil, err
				}
				for j := range next {
					if !bytes.Equal(next[j].raw, sub[j].raw) {
						out[i].raw = objectBytes(next)
						break
					}
				}
			}
			continue
		}
		nv, ok := values[path]
		if !ok {
			continue
		}
		kind := kinds[path]
		switch kind {
		case "fixed":
			continue
		case "secret":
			if s, _ := nv.(string); s == "" {
				continue
			}
		}
		raw, err := encodeValue(d.format, kind, v.raw, nv)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", humanLabel(v.key), err)
		}
		out[i].raw = raw
	}
	return out, nil
}

// objectBytes writes settings back as a JSON object in their own order.
func objectBytes(vals []cfgValue) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, v := range vals {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(v.key)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v.raw)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func encodeValue(format, kind string, old json.RawMessage, v any) (json.RawMessage, error) {
	quotedBefore := bytes.HasPrefix(old, []byte(`"`))
	asText := func(s string) (json.RawMessage, error) { return json.Marshal(s) }
	switch kind {
	case "switch":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be on or off")
		}
		if format == "kv" || quotedBefore {
			return asText(strconv.FormatBool(b))
		}
		return json.Marshal(b)
	case "number":
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return nil, errors.New("must be a number")
			}
			n = f
		default:
			return nil, errors.New("must be a number")
		}
		s := strconv.FormatFloat(n, 'f', -1, 64)
		if format == "kv" || quotedBefore {
			return asText(s)
		}
		return json.RawMessage(s), nil
	case "list":
		var items []string
		switch x := v.(type) {
		case []any:
			for _, it := range x {
				if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
					items = append(items, strings.TrimSpace(s))
				}
			}
		case string:
			for _, s := range strings.Split(x, ",") {
				if s = strings.TrimSpace(s); s != "" {
					items = append(items, s)
				}
			}
		}
		if format == "kv" || quotedBefore {
			return asText(strings.Join(items, ","))
		}
		if items == nil {
			items = []string{}
		}
		return json.Marshal(items)
	default:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be text")
		}
		return asText(s)
	}
}

/* --------------------------------------------------------- declared schema */

// declaredSchema reads manifest.config (Stremio SDK) into fields and an
// initial document of their defaults.
func declaredSchema(raw json.RawMessage) ([]AddonConfigField, cfgDoc, bool) {
	var man struct {
		Config []struct {
			Key      string   `json:"key"`
			Type     string   `json:"type"`
			Title    string   `json:"title"`
			Options  []string `json:"options"`
			Required bool     `json:"required"`
			Default  any      `json:"default"`
		} `json:"config"`
	}
	if json.Unmarshal(raw, &man) != nil || len(man.Config) == 0 {
		return nil, cfgDoc{}, false
	}
	doc := cfgDoc{format: "json"}
	var fields []AddonConfigField
	for _, c := range man.Config {
		if !kvKey.MatchString(c.Key) {
			continue
		}
		f := AddonConfigField{Key: c.Key, Label: c.Title, Required: c.Required}
		if f.Label == "" {
			f.Label = humanLabel(c.Key)
		}
		def := c.Default
		switch c.Type {
		case "password":
			f.Kind = "secret"
		case "number":
			f.Kind = "number"
		case "checkbox":
			f.Kind = "switch"
			if def == nil {
				def = false
			}
		case "select":
			f.Kind = "select"
			for _, o := range c.Options {
				f.Options = append(f.Options, AddonConfigOption{Value: o, Label: o})
			}
		default:
			f.Kind = "text"
		}
		if def == nil {
			def = ""
		}
		if f.Kind != "secret" {
			f.Value = def
		}
		rawDef, _ := json.Marshal(def)
		doc.values = append(doc.values, cfgValue{key: c.Key, raw: rawDef})
		fields = append(fields, f)
	}
	return fields, doc, len(fields) > 0
}

/* ----------------------------------------------------------- the manager */

// addonState is what Ether knows about an add-on's configuration.
type addonState struct {
	rec    Record
	link   addonLink
	doc    cfgDoc
	fields []AddonConfigField
	layer  string
	hints  stremio.BehaviorHints
}

func (m *Manager) addonState(ctx context.Context, id string) (addonState, error) {
	rec, err := m.store.record(ctx, id)
	if err != nil {
		return addonState{}, err
	}
	if rec.SourceURL == "" || !rec.Manifest.SpeaksStremio() {
		return addonState{}, ErrNoAddonLink
	}
	link, err := parseAddonLink(rec.SourceURL)
	if err != nil {
		return addonState{}, err
	}
	st := addonState{rec: rec, link: link, hints: rec.Manifest.BehaviorHints, layer: "none"}
	raw := rec.ManifestRaw
	if fields, doc, ok := declaredSchema(raw); ok {
		st.layer, st.fields, st.doc = "declared", fields, doc
		if d, ok := decodeSegment(link.segment); ok && d.format == "json" {
			st.doc = mergeDeclared(doc, d)
			st.fields = refreshDeclared(fields, st.doc)
		}
		return st, nil
	}
	if d, ok := decodeSegment(link.segment); ok {
		st.layer, st.doc, st.fields = "decoded", d, fieldsOf(d)
		return st, nil
	}
	if link.segment != "" && !m.segmentIsPrefix(ctx, link, rec) {
		st.layer = "page"
		return st, nil
	}
	// No settings in the link: an unconfigured add-on, or one whose last
	// path segment is just where it is hosted.
	st.link = link.asRoot()
	if st.hints.Configurable || st.hints.ConfigurationRequired {
		st.layer = "page"
	}
	return st, nil
}

// segmentIsPrefix says whether the link's last segment is part of where the
// add-on lives (it answers without it differently) rather than settings.
func (m *Manager) segmentIsPrefix(ctx context.Context, l addonLink, rec Record) bool {
	if strings.Contains(l.segment, ".") && !strings.ContainsAny(l.segment, "=%{") {
		// Host-like segments ("publicdomainmovies.now.sh") are paths.
		return true
	}
	body, err := m.fetch(ctx, l.withSegment(""), false, 1<<20)
	if err != nil {
		return true
	}
	var man struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &man) != nil || man.ID == "" {
		return true
	}
	var mine struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.ManifestRaw, &mine)
	return man.ID != mine.ID
}

func mergeDeclared(defaults, saved cfgDoc) cfgDoc {
	out := defaults
	out.values = make([]cfgValue, len(defaults.values))
	copy(out.values, defaults.values)
	byKey := map[string]json.RawMessage{}
	for _, v := range saved.values {
		byKey[v.key] = v.raw
	}
	for i, v := range out.values {
		if raw, ok := byKey[v.key]; ok {
			out.values[i].raw = raw
		}
	}
	return out
}

func refreshDeclared(fields []AddonConfigField, doc cfgDoc) []AddonConfigField {
	byKey := map[string]json.RawMessage{}
	for _, v := range doc.values {
		byKey[v.key] = v.raw
	}
	out := make([]AddonConfigField, len(fields))
	copy(out, fields)
	for i, f := range out {
		raw := byKey[f.Key]
		var v any
		_ = json.Unmarshal(raw, &v)
		if f.Kind == "secret" {
			s, _ := v.(string)
			out[i].Set = s != ""
			continue
		}
		out[i].Value = v
	}
	return out
}

// AddonConfig describes an installed add-on's settings for the console.
func (m *Manager) AddonConfig(ctx context.Context, id string) (AddonConfig, error) {
	st, err := m.addonState(ctx, id)
	if err != nil {
		return AddonConfig{}, err
	}
	fields := st.fields
	if fields == nil {
		fields = []AddonConfigField{}
	}
	return AddonConfig{
		Configurable: st.hints.Configurable || st.hints.ConfigurationRequired || st.layer == "declared" || st.layer == "decoded",
		Required:     st.hints.ConfigurationRequired,
		Configured:   st.link.segment != "",
		Layer:        st.layer,
		Fields:       fields,
		Host:         displayHost(st.link.root.Hostname()),
	}, nil
}

// AddonConfigChange is either new values for the drawn fields or a whole
// install link (from the add-on's own configure page, or pasted).
type AddonConfigChange struct {
	Values map[string]any `json:"values"`
	Link   string         `json:"link"`
}

// configuredLink builds the install link a change asks for.
func (m *Manager) configuredLink(ctx context.Context, id string, ch AddonConfigChange) (addonState, string, error) {
	st, err := m.addonState(ctx, id)
	if err != nil {
		return st, "", err
	}
	if strings.TrimSpace(ch.Link) != "" {
		link := strings.TrimSpace(ch.Link)
		if strings.HasPrefix(strings.ToLower(link), "stremio://") {
			link = "https://" + link[len("stremio://"):]
		}
		next, err := parseAddonLink(link)
		if err != nil {
			return st, "", &ValidationError{[]FieldError{{"link", "That is not an add-on install link."}}}
		}
		out := next.withSegment(next.segment)
		if !strings.EqualFold(next.root.Host, st.link.root.Host) && !m.sameAddon(ctx, st.link.withSegment(st.link.segment), out) {
			return st, "", &ValidationError{[]FieldError{{"link", "That link is for a different add-on."}}}
		}
		return st, out, nil
	}
	if st.layer != "declared" && st.layer != "decoded" {
		return st, "", &ValidationError{[]FieldError{{"values", "Use the add-on's own settings page for this one."}}}
	}
	doc, err := st.doc.apply(st.fields, ch.Values)
	if err != nil {
		return st, "", &ValidationError{[]FieldError{{"values", err.Error()}}}
	}
	seg, err := doc.encode()
	if err != nil {
		return st, "", err
	}
	return st, st.link.withSegment(seg), nil
}

// AddonConfigResult is what a configuration would change, before saving.
type AddonConfigResult struct {
	Name     string   `json:"name"`
	Added    []string `json:"added,omitempty"`   // catalogues it would add
	Removed  []string `json:"removed,omitempty"` // catalogues it would drop
	Roles    []string `json:"roles"`             // what it would do in Ether
	Sample   string   `json:"sample,omitempty"`  // a live result, in words
	Problems []string `json:"problems,omitempty"`
}

// sampleTitle is the public-domain film Ether asks for when trying sources.
const sampleTitle = "tt0063350"

// TestAddonConfig loads the add-on with the new settings without saving.
func (m *Manager) TestAddonConfig(ctx context.Context, id string, ch AddonConfigChange) (AddonConfigResult, error) {
	st, link, err := m.configuredLink(ctx, id, ch)
	if err != nil {
		return AddonConfigResult{}, err
	}
	return m.tryAddonLink(ctx, st.rec.Manifest, link)
}

// tryAddonLink fetches an add-on's manifest at link and one live result.
// sameAddon reports whether a link on another host of the same site (an
// add-on that moved from peerflix.mov to addon.peerflix.mov) serves the
// same add-on. Shared hosting (many add-ons under one site) is told apart
// by the manifest id.
func (m *Manager) sameAddon(ctx context.Context, current, next string) bool {
	a, errA := url.Parse(current)
	b, errB := url.Parse(next)
	if errA != nil || errB != nil || siteOf(a.Hostname()) != siteOf(b.Hostname()) {
		return false
	}
	id := func(link string) string {
		body, err := m.fetch(ctx, link, false, 4<<20)
		if err != nil {
			return ""
		}
		var man struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &man) != nil {
			return ""
		}
		return man.ID
	}
	was := id(current)
	return was != "" && was == id(next)
}

func (m *Manager) tryAddonLink(ctx context.Context, before Manifest, link string) (AddonConfigResult, error) {
	body, err := m.fetch(ctx, link, false, 4<<20)
	if err != nil {
		return AddonConfigResult{}, &ValidationError{[]FieldError{{"link", "The add-on didn't answer with these settings."}}}
	}
	man, err := ParseManifest(body)
	if err != nil {
		return AddonConfigResult{}, &ValidationError{[]FieldError{{"link", "The add-on answered with something Ether can't read."}}}
	}
	res := AddonConfigResult{Name: man.Name}
	was := map[string]bool{}
	for _, c := range before.Catalogs {
		was[c.Type+"/"+c.ID] = true
	}
	now := map[string]bool{}
	for _, c := range man.Catalogs {
		now[c.Type+"/"+c.ID] = true
		if !was[c.Type+"/"+c.ID] {
			res.Added = append(res.Added, catalogLabel(c))
		}
	}
	for _, c := range before.Catalogs {
		if !now[c.Type+"/"+c.ID] {
			res.Removed = append(res.Removed, catalogLabel(c))
		}
	}
	for _, c := range man.AddonRoles() {
		res.Roles = append(res.Roles, string(c))
	}
	if man.ConfigurationRequired() {
		res.Problems = append(res.Problems, "The add-on still asks to be configured.")
	}
	res.Sample = m.sampleAddon(ctx, man, link)
	return res, nil
}

func catalogLabel(c StremioCat) string {
	name := c.Name
	if name == "" {
		name = c.ID
	}
	return name + " · " + c.Type
}

// sampleAddon asks the add-on for one real thing and says what came back.
func (m *Manager) sampleAddon(ctx context.Context, man Manifest, link string) string {
	l, err := parseAddonLink(link)
	if err != nil {
		return ""
	}
	base := strings.TrimSuffix(l.withSegment(l.segment), "/manifest.json")
	if i := strings.Index(base, "?"); i >= 0 {
		base = base[:i]
	}
	get := func(route string) (map[string]json.RawMessage, bool) {
		body, err := m.fetch(ctx, base+route, false, 4<<20)
		if err != nil {
			return nil, false
		}
		var doc map[string]json.RawMessage
		return doc, json.Unmarshal(body, &doc) == nil
	}
	count := func(doc map[string]json.RawMessage, key string) int {
		var arr []json.RawMessage
		_ = json.Unmarshal(doc[key], &arr)
		return len(arr)
	}
	if man.Serves(stremio.ResourceStream, stremio.TypeMovie) {
		if doc, ok := get("/stream/movie/" + sampleTitle + ".json"); ok {
			return fmt.Sprintf("Found %d sources for Night of the Living Dead (1968).", count(doc, "streams"))
		}
		return "Couldn't fetch sources for a sample film."
	}
	for _, c := range man.Catalogs {
		required := false
		for _, x := range c.Extras() {
			required = required || x.IsRequired
		}
		if required {
			continue
		}
		if doc, ok := get(stremio.Route("catalog", c.Type, c.ID, nil)); ok {
			return fmt.Sprintf("%s lists %d titles.", catalogLabel(c), count(doc, "metas"))
		}
		return "Couldn't fetch a sample catalogue."
	}
	if man.HasResource("subtitles") {
		if doc, ok := get("/subtitles/movie/" + sampleTitle + ".json"); ok {
			return fmt.Sprintf("Found %d subtitles for Night of the Living Dead (1968).", count(doc, "subtitles"))
		}
	}
	return ""
}

// SaveAddonConfig applies new settings in place: the plugin keeps its id,
// roles, pins and collections; only its sealed link and manifest change.
func (m *Manager) SaveAddonConfig(ctx context.Context, id string, ch AddonConfigChange) (Record, error) {
	st, link, err := m.configuredLink(ctx, id, ch)
	if err != nil {
		return Record{}, err
	}
	rec := st.rec
	if !rec.Manifest.IsStremioOnly() {
		// A Studio-edited add-on runs its own code; only the link it asks
		// changes.
		if _, err := m.tryAddonLink(ctx, rec.Manifest, link); err != nil {
			return Record{}, err
		}
		rec.SourceURL = link
		if err := m.store.upsert(ctx, rec); err != nil {
			return Record{}, err
		}
		m.closeRuntime(id)
		m.broadcast(ctx)
		return m.store.record(ctx, id)
	}
	p, err := m.PreviewInstall(ctx, Source{Kind: "url", URL: link})
	if err != nil {
		return Record{}, err
	}
	p.Manifest.ID = rec.ID
	saved, err := m.Install(ctx, p, InstallOptions{AutoUpdate: rec.AutoUpdate, Shadow: rec.Shadow})
	if err != nil {
		return Record{}, err
	}
	m.closeRuntime(id)
	return saved, nil
}

/* ------------------------------------------------------------------- HTTP */

func (m *Manager) handleGetAddonConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := m.AddonConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (m *Manager) handleTestAddonConfig(w http.ResponseWriter, r *http.Request) {
	var ch AddonConfigChange
	if err := decode(r, &ch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	res, err := m.TestAddonConfig(r.Context(), r.PathValue("id"), ch)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (m *Manager) handleSaveAddonConfig(w http.ResponseWriter, r *http.Request) {
	var ch AddonConfigChange
	if err := decode(r, &ch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	rec, err := m.SaveAddonConfig(r.Context(), r.PathValue("id"), ch)
	if err != nil {
		m.fail(w, err)
		return
	}
	h, _ := m.store.health(r.Context(), rec.ID)
	writeJSON(w, http.StatusOK, m.summary(rec, h))
}

// displayHost is how a person reads where an add-on lives. Hosting
// services hand add-ons generated names
// (7a82163c306e-stremio-netflix-catalog-addon.baby-beamup.club); those read
// as their site (baby-beamup.club).
func displayHost(h string) string {
	first, _, _ := strings.Cut(h, ".")
	if len(first) > 24 {
		return siteOf(h)
	}
	return h
}
