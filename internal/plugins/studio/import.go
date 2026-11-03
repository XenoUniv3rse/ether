package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/plugins"
)

// ErrNotEditable reports a plugin whose code does not live on this server
// (a sidecar on another device).
var ErrNotEditable = errors.New("studio: this plugin runs on another device; its code is not on this server")

// ImportPlugin opens an installed plugin in Studio: its newest draft when
// it has one, otherwise a new draft built from what is installed. A
// Stremio add-on becomes a native plugin that answers the same routes
// through the add-on, so editing it has no limits a native plugin lacks.
func (s *Studio) ImportPlugin(ctx context.Context, pluginID string) (*Project, error) {
	if s.mgr == nil {
		return nil, fmt.Errorf("studio: plugin manager is not configured")
	}
	if p := s.newestDraftFor(pluginID); p != nil {
		return p, nil
	}
	rec, err := s.mgr.Get(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	switch {
	case rec.Manifest.IsStremioOnly():
		files, err := addonProjectFiles(rec)
		if err != nil {
			return nil, err
		}
		return s.createFromFiles(ctx, rec.Name, rec.ID, "addon", rec.ID, files, nil, true)
	case rec.Manifest.RuntimeKind() == "hosted":
		pkg, err := s.mgr.PackageFiles(ctx, rec.ID)
		if err != nil {
			return nil, err
		}
		files := sourceFiles(pkg)
		addonOf := ""
		if rec.Manifest.Ether != nil && rec.Manifest.Ether.Stremio {
			addonOf = rec.ID
		}
		return s.createFromFiles(ctx, rec.Name, rec.ID, templateFor(rec.Manifest, addonOf), addonOf, files, nil, true)
	default:
		return nil, ErrNotEditable
	}
}

func (s *Studio) newestDraftFor(pluginID string) *Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Project
	for _, p := range s.projects {
		if p.PluginID == pluginID && (best == nil || p.UpdatedAt.After(best.UpdatedAt)) {
			best = p
		}
	}
	if best == nil {
		return nil
	}
	return cloneProject(best)
}

func templateFor(m plugins.Manifest, addonOf string) string {
	switch {
	case addonOf != "":
		return "addon"
	case m.HasCapability(plugins.CapLiveTV):
		return "channels"
	case m.HasCapability(plugins.CapStreamSource):
		return "video_source"
	case m.HasCapability(plugins.CapListSource):
		return "list"
	}
	return "plugin"
}

// sourceFiles keeps an installed package's editable text files. A Studio
// build installs its compiled bundle beside the source; the draft goes
// back to the source entry.
func sourceFiles(pkg map[string][]byte) map[string]string {
	out := map[string]string{}
	for name, b := range pkg {
		if strings.HasPrefix(name, "dist/") || strings.HasSuffix(name, ".map") || !utf8.Valid(b) {
			continue
		}
		switch path.Ext(name) {
		case ".ts", ".tsx", ".js", ".json", ".md":
			out[name] = string(b)
		}
	}
	if raw, ok := out["manifest.json"]; ok {
		var doc map[string]any
		if json.Unmarshal([]byte(raw), &doc) == nil {
			if ether, _ := doc["ether"].(map[string]any); ether != nil {
				if rt, _ := ether["runtime"].(map[string]any); rt != nil {
					if entry, _ := rt["entry"].(string); entry == installBundlePath {
						if _, ok := out["src/plugin.ts"]; ok {
							rt["entry"] = "src/plugin.ts"
							out["manifest.json"] = prettyJSON(doc)
						}
					}
				}
			}
		}
	}
	return out
}

// createFromFiles stores a draft for pluginID from ready files.
func (s *Studio) createFromFiles(ctx context.Context, name, pluginID, template, addonOf string, files, overrides map[string]string, installed bool) (*Project, error) {
	merged := map[string]string{}
	for k, v := range files {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	comp := s.CompileFiles(merged, "")
	defaultPage := ""
	if man, err := plugins.ParseManifest([]byte(merged["manifest.json"])); err == nil && man.Ether != nil {
		if pages := man.Ether.Contributes.Pages; len(pages) > 0 {
			defaultPage = pages[0].Key
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	projID := pluginID
	for s.projects[projID] != nil {
		s.seq++
		projID = fmt.Sprintf("%s-%d", pluginID, s.seq)
	}
	now := s.now()
	p := &Project{
		ID: projID, Template: template, Name: name, PluginID: pluginID, AddonOf: addonOf,
		Installed: installed, Files: merged, Shadow: true, Settings: map[string]any{},
		Diagnostics: comp.Diagnostics, DefaultPage: defaultPage, CreatedAt: now, UpdatedAt: now,
		kv: map[string][]byte{}, calls: []plugins.CallRecord{}, logs: []string{},
		journal: []plugins.JournalEntry{}, collections: map[string]json.RawMessage{},
		watchState: map[string]bool{},
	}
	if err := s.persistLocked(ctx, p); err != nil {
		return nil, err
	}
	s.projects[p.ID] = p
	return cloneProject(p), nil
}

// addonProjectFiles writes the native plugin equivalent of an add-on: its
// own Stremio manifest (types, catalogues, extras, resources, id prefixes)
// with an Ether block that runs code in Ether and answers through the
// add-on, plus code with clear places to change what it returns.
func addonProjectFiles(rec plugins.Record) (map[string]string, error) {
	var doc map[string]any
	if err := json.Unmarshal(rec.ManifestRaw, &doc); err != nil || doc == nil {
		raw, err := json.Marshal(rec.Manifest)
		if err != nil {
			return nil, err
		}
		doc = map[string]any{}
		_ = json.Unmarshal(raw, &doc)
	}
	delete(doc, "ether")
	doc["id"] = rec.ID
	doc["version"] = nextPatch(rec.Version)
	// Roles come from the Stremio fields above (catalogs, resources), so
	// the Ether block only says how the code runs.
	ether := map[string]any{
		"protocol": 1,
		"stremio":  true,
		"runtime":  map[string]any{"kind": "hosted", "entry": "src/plugin.ts"},
		"summary":  "$t:summary",
		"strings":  map[string]any{"en": "i18n/en.json"},
		"author":   map[string]any{"name": "Server owner"},
		"license":  "MIT",
	}
	if prev := rec.Manifest.Ether; prev != nil && prev.Author.Name != "" {
		ether["author"] = map[string]any{"name": prev.Author.Name}
	}
	doc["ether"] = ether
	// A native manifest follows the console's copy rules; the add-on's own
	// text can be long or shouty.
	if d, _ := doc["description"].(string); d != "" {
		doc["description"] = plainCopy(d, 300)
	}

	summary := plainCopy(rec.Manifest.Description, 80)
	if summary == "" {
		summary = plainCopy(rec.Name, 80)
	}
	text := map[string]string{"summary": summary}
	return map[string]string{
		"manifest.json": prettyJSON(doc),
		"i18n/en.json":  prettyJSON(text),
		"src/plugin.ts": addonPluginSource(rec),
	}, nil
}

// plainCopy fits third-party text to the console's copy rules.
func plainCopy(s string, max int) string {
	s = strings.NewReplacer("!", "", "—", ", ", "–", "-").Replace(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		r := []rune(s)[:max-3]
		cut := string(r)
		if i := strings.LastIndex(cut, " "); i > max/2 {
			cut = cut[:i]
		}
		s = strings.TrimRight(strings.TrimSpace(cut), ",.;:") + "..."
	}
	if s != "" {
		r := []rune(s)
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		s = string(r)
	}
	return s
}

func nextPatch(v string) string {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) == 3 {
		patch := parts[2]
		suffix := ""
		if i := strings.IndexAny(patch, "-+"); i >= 0 {
			patch, suffix = patch[:i], ""
		}
		if n, err := strconv.Atoi(patch); err == nil {
			return fmt.Sprintf("%s.%s.%d%s", parts[0], parts[1], n+1, suffix)
		}
	}
	return "1.0.1"
}

func addonPluginSource(rec plugins.Record) string {
	name := strings.ReplaceAll(rec.Name, "*/", "")
	return `// ` + name + `, opened from its Stremio add-on in Studio.
// Ether calls the same add-on routes as before: catalogues, metadata,
// streams and subtitles. Each one is answered by the add-on itself through
// ether.addon.get, so this plugin behaves exactly like the original until
// you change it. To drop or add a catalogue, edit "catalogs" in
// manifest.json: Ether offers exactly the catalogues listed there. Edit the
// functions below to filter, rename or merge what the add-on returns. The
// add-on's link stays sealed in Ether.

type Request = {
  method: string;
  path: string;
  query?: Record<string, string[] | string>;
};

/** Change a catalogue page before Ether sees it. */
function editCatalog(type: string, id: string, metas: any[]): any[] {
  return metas;
}

/** Change one title's details. */
function editMeta(type: string, id: string, meta: any): any {
  return meta;
}

/** Change the playable sources found for a title. */
function editStreams(type: string, id: string, streams: any[]): any[] {
  return streams;
}

/** Change the subtitles offered for a title. */
function editSubtitles(type: string, id: string, subtitles: any[]): any[] {
  return subtitles;
}

function addon(route: string): any {
  const res = ether.addon.get(route);
  if (res.status !== 200) throw new Error("The add-on answered HTTP " + res.status);
  return JSON.parse(res.body);
}

const ROUTE = /^\/(catalog|meta|stream|subtitles|addon_catalog)\/([^/]+)\/([^/]+?)(?:\/[^/]*)?\.json$/;

export function handle(req: Request) {
  if (req.path === "/ether/health") {
    return { status: 200, body: { status: "ok", reason: "" } };
  }
  if (req.path === "/ether/configure") {
    return { status: 200, body: { errors: {} } };
  }
  if (req.path === "/ether/test") {
    try {
      addon("/manifest.json");
      return { status: 200, body: { ok: true, message: "The add-on answers." } };
    } catch (e) {
      return { status: 200, body: { ok: false, message: String(e) } };
    }
  }
  const m = ROUTE.exec(req.path);
  if (!m) {
    return { status: 404, body: { error: { code: "not_found", message: "Route not found." } } };
  }
  const resource = m[1]!;
  const type = decodeURIComponent(m[2]!);
  const id = decodeURIComponent(m[3]!);
  let body: any;
  try {
    body = addon(req.path);
  } catch (e) {
    return { status: 502, body: { error: { code: "upstream", message: String(e) } } };
  }
  if (resource === "catalog") body.metas = editCatalog(type, id, body.metas || []);
  if (resource === "meta" && body.meta) body.meta = editMeta(type, id, body.meta);
  if (resource === "stream") body.streams = editStreams(type, id, body.streams || []);
  if (resource === "subtitles") body.subtitles = editSubtitles(type, id, body.subtitles || []);
  return { status: 200, body };
}

(globalThis as any).handle = handle;
`
}
