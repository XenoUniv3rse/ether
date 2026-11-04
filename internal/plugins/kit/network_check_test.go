package kit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestOfflineCheckDoesNotInventUpstreamResponse(t *testing.T) {
	raw, files := baseGoodFiles()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	e := m["ether"].(map[string]any)
	e["capabilities"] = []string{"list_source"}
	e["contributes"] = map[string]any{"listSources": []map[string]any{{"key": "shows", "label": "Shows", "catalog": "series/shows"}}}
	raw, _ = json.Marshal(m)
	for _, tc := range []struct {
		name, host, code string
		ok, skip         bool
	}{
		{"declared upstream", "api.example.com", `const shows=JSON.parse(ether.http.fetch({url:"https://api.example.com/shows"}).body); return {status:200,body:{metas:shows.map(s=>({id:s.id,name:s.name}))}};`, true, true},
		{"undeclared upstream", "evil.example", `ether.http.fetch({url:"https://evil.example/shows"});return {status:200,body:{metas:[]}};`, false, false},
		{"independent bug", "", `throw new Error("real failure");`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files["dist/plugin.js"] = []byte(`globalThis.handle=req=>{if(req.path==="/ether/health")return {status:200,body:{status:"ok"}};if(req.path==="/ether/configure")return {status:200,body:{errors:{}}};if(req.path==="/catalog/series/shows.json"){` + tc.code + `}return {status:404,body:{error:{code:"not_found",message:"missing"}}};};`)
			rep := Check(context.Background(), &Package{ManifestRaw: raw, Files: files})
			if rep.OK != tc.ok {
				t.Fatalf("OK=%v: %s", rep.OK, rep.Text())
			}
			skipped := false
			for _, r := range rep.Routes {
				if strings.HasPrefix(r.Route, "/catalog/") {
					if r.Route != "/catalog/series/shows.json" {
						t.Fatalf("wrong typed catalogue path: %s", r.Route)
					}
					skipped = r.Skipped
					if r.Skipped && r.OK {
						t.Fatal("unexecuted route reported as passing")
					}
				}
			}
			if skipped != tc.skip {
				t.Fatalf("skipped=%v: %s", skipped, rep.Text())
			}
			if tc.skip && len(rep.Warnings) == 0 {
				t.Fatal("missing live-test warning")
			}
		})
	}
}
