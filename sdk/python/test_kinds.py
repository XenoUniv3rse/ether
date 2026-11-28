"""Content-kind conformance: replay sdk/conformance/kinds/fixture.json."""

import copy
import json
import pathlib
import unittest
import urllib.parse

from ether_plugin import (
    CatalogProvider,
    FrontendAdapter,
    KindMatcher,
    KindResolver,
    SetupTester,
    candidate_ref,
    create_mock_host_client,
    parse_feed,
    plugin,
)

FIXTURE = json.loads(
    (pathlib.Path(__file__).parent / ".." / "conformance" / "kinds" / "fixture.json").read_text()
)


def _page(items, req):
    items = sorted(items, key=lambda it: it.get("index", 0))
    limit = int(req.get("limit") or 200)
    off = int(req.get("cursor") or 0)
    out = {"items": items[off:off + limit]}
    if off + limit < len(items):
        out["nextCursor"] = str(off + limit)
    return out


class Dataset(CatalogProvider, KindResolver, KindMatcher):
    def __init__(self, items):
        self.items = items

    def by_id(self, item_id):
        return next((it for it in self.items if it["id"] == item_id), None)

    def browse(self, ctx, req):
        return _page([it for it in self.items
                      if it["level"] == req.get("level") and it.get("parentId", "") == (req.get("parentId") or "")], req)

    def search(self, ctx, req):
        q = str(req.get("query") or "").lower()
        return _page([it for it in self.items if q in it["title"].lower()], req)

    def detail(self, ctx, kind, item_id):
        return copy.deepcopy(self.by_id(item_id))

    def changes(self, ctx, req):
        return {"upserts": copy.deepcopy(self.items), "complete": True}

    def resolve(self, ctx, req):
        item, hints = req["item"], req["item"].get("hints") or {}
        sources = []
        parent = self.by_id(item.get("parentId", ""))
        if parent and hints.get("member"):
            ph = parent.get("hints") or {}
            sources.append({
                "id": "zip", "pattern": "container", "mime": "audio/flac",
                "container": {"format": "zip",
                              "volumes": [{"url": ph["zipUrl"], "size": ph["zipSize"]}],
                              "member": hints["member"]},
            })
        if hints.get("pageUrl"):
            sources.append({"id": "direct", "pattern": "range", "mime": "audio/flac", "url": hints["pageUrl"]})
        return {"sources": sources}

    def match(self, ctx, req):
        matches = []
        for c in req.get("candidates") or []:
            hit = req["item"]["title"].lower() in str(c.get("title", "")).lower()
            matches.append({"candidateRef": candidate_ref(c),
                            "score": 0.95 if hit else 0.1,
                            "reasons": ["Title matches" if hit else "Title does not match"]})
        return {"matches": sorted(matches, key=lambda m: -m["score"])}


class Setup(SetupTester):
    def test_setup(self, ctx, req):
        if (req.get("fields") or {}).get("apiKey"):
            return {"ok": True, "detail": "Connected"}
        return {"ok": False, "detail": "Add your API key first", "fields": {"apiKey": "Required"}}


class Feed(FrontendAdapter):
    def handle(self, method, path, headers, body):
        return 200, {"Content-Type": "application/atom+xml"}, '<?xml version="1.0"?><feed><title>Field Recordings</title></feed>'


class Recorder(FrontendAdapter):
    def handle(self, method, path, headers, body):
        self.seen = (method, path)
        return 200, {"X-Seen-Method": method, "X-Seen-Path": path}, "body:" + path


class FrontendDispatchTest(unittest.TestCase):
    def _plugin(self):
        inst = plugin(
            manifest={"id": "community.example.dav", "name": "DAV", "version": "1.0.0",
                      "ether": {"protocol": 1, "kind": "content", "summary": "DAV",
                                "runtime": {"kind": "remote"},
                                "capabilities": ["catalog_provider", "frontend_adapter"]}},
            host=create_mock_host_client(),
        )
        rec = Recorder()
        inst.on_frontend("dav", rec)
        return inst, rec

    def test_frontend_accepts_any_method_and_keeps_query(self):
        inst, rec = self._plugin()
        resp = inst.dispatch({"method": "PROPFIND", "path": "/ether/v1/frontend/dav/library/"})
        self.assertEqual(resp["status"], 200, resp)
        self.assertEqual(rec.seen[0], "PROPFIND")
        self.assertEqual(rec.seen[1], "/library/")

        resp = inst.dispatch({"method": "GET", "path": "/ether/v1/frontend/dav/rest/ping.view?u=a&t=b"})
        self.assertEqual(resp["status"], 200, resp)
        self.assertEqual(rec.seen[0], "GET")
        query = urllib.parse.parse_qs(urllib.parse.urlparse(rec.seen[1]).query)
        self.assertEqual(query.get("u"), ["a"])
        self.assertEqual(query.get("t"), ["b"])
        self.assertTrue(rec.seen[1].startswith("/rest/ping.view"))

    def test_head_returns_no_body(self):
        inst, _ = self._plugin()
        resp = inst.dispatch({"method": "HEAD", "path": "/ether/v1/frontend/dav/library/"})
        self.assertEqual(resp["status"], 200, resp)
        self.assertEqual(resp["body"], b"")

    def test_kinds_still_refuses_non_get_post(self):
        inst, _ = self._plugin()
        resp = inst.dispatch({"method": "PUT", "path": "/ether/v1/kinds/recordings/browse"})
        self.assertEqual(resp["status"], 405)


class KindsConformanceTest(unittest.TestCase):
    def test_fixture_replay(self):
        inst = plugin(
            manifest={"id": "community.example.recordings", "name": "Field Recordings", "version": "1.0.0",
                      "ether": {"protocol": 1, "kind": "content", "summary": "Fixture provider",
                                "runtime": {"kind": "remote"},
                                "capabilities": ["catalog_provider", "kind_resolver", "matcher", "frontend_adapter"],
                                "contributes": {"mediaKinds": [FIXTURE["kind"]]}}},
            host=create_mock_host_client(),
        )
        ds = Dataset(FIXTURE["items"])
        inst.on_catalog("recordings", ds)
        inst.on_resolve("recordings", ds)
        inst.on_match("recordings", ds)
        inst.on_setup_test(Setup())
        inst.on_frontend("feed", Feed())

        for call in FIXTURE["calls"]:
            with self.subTest(call=call["name"]):
                resp = inst.dispatch({"method": call["method"], "path": call["path"],
                                      "body": json.dumps(call.get("body") or {})})
                self.assertEqual(resp["status"], call["expectStatus"], resp)
                body = resp["body"]
                if "expect" in call:
                    if isinstance(body, (str, bytes)):
                        body = json.loads(body)
                    self.assertEqual(body, call["expect"])
                elif "expectIds" in call:
                    self.assertEqual([it["id"] for it in body["upserts"]], call["expectIds"])
                    self.assertEqual(body["complete"], call["expectComplete"])
                elif "expectTopRef" in call:
                    self.assertEqual(body["matches"][0]["candidateRef"], call["expectTopRef"])
                elif "expectBodyContains" in call:
                    self.assertIn(call["expectBodyContains"], str(body))

    def test_mock_host_kinds_and_feed(self):
        host = create_mock_host_client()
        self.assertEqual(host.kinds.upsert("recordings", [{"id": "a", "title": "A"}]), 1)
        self.assertEqual(len(host.kinds.items("recordings")["items"]), 1)
        ref = host.cache.put("pk-x-recordings", "a.flac", b"bytes")
        self.assertTrue(ref.startswith("blob:"))
        self.assertEqual(host.cache.open(ref), b"bytes")
        host.kinds.delete("recordings", ["a"])
        self.assertEqual(host.kinds.items("recordings")["items"], [])
        feed = parse_feed('<rss><channel><title>T</title><item><title>E</title><guid>g1</guid><enclosure url="https://x/e.mp3"/></item></channel></rss>')
        self.assertEqual(feed["entries"][0]["audio"], "https://x/e.mp3")

    def test_stream_url_host_service(self):
        from ether_plugin import PluginContext, RemoteHostClient, stream_url

        host = create_mock_host_client()
        ctx = PluginContext(
            manifest={},
            settings={},
            locale="en",
            pick_token=None,
            request_id=None,
            headers={"X-Ether-Viewer": "7"},
            query={},
            ether=host,
            i18n=None,
        )
        url = ctx.stream_url("pk:recordings:a")
        self.assertTrue(url.startswith("/ether/v1/kind-media/"))
        self.assertEqual(host.last_stream_url, (7, "pk:recordings:a"))
        self.assertIsNone(host.last_stream_url_part)
        self.assertEqual(stream_url(ctx, "pk:recordings:a"), url)

        part_url = ctx.stream_url("pk:recordings:a", 2)
        self.assertTrue(part_url.endswith("?part=2"))
        self.assertEqual(host.last_stream_url_part, 2)
        self.assertIsNone(host.last_stream_url_source)
        self.assertEqual(stream_url(ctx, "pk:recordings:a", 2), part_url)
        with self.assertRaises(ValueError):
            ctx.stream_url("pk:recordings:a", -1)

        src_url = ctx.stream_url("pk:recordings:a", 1, source="pdf", fallback=True)
        self.assertTrue(src_url.endswith("?part=1"))
        self.assertEqual(host.last_stream_url_part, 1)
        self.assertEqual(host.last_stream_url_source, "pdf")
        self.assertTrue(host.last_stream_url_fallback)
        self.assertEqual(stream_url(ctx, "pk:recordings:a", source="epub"), "/ether/v1/kind-media/mock-7")
        self.assertEqual(host.last_stream_url_source, "epub")
        with self.assertRaises(ValueError):
            ctx.stream_url("pk:recordings:a", source="   ")

        seen = []
        remote = RemoteHostClient(base_url="http://ether.local", credential="cred")
        remote.frontend._call = lambda method, path, body=None, allow_404=False: (  # type: ignore[method-assign]
            seen.append((method, path, body)) or {"url": "/ether/v1/kind-media/remote-part"}
        )
        rctx = PluginContext(
            manifest={},
            settings={},
            locale="en",
            pick_token=None,
            request_id=None,
            headers={"X-Ether-Viewer": "7"},
            query={},
            ether=remote,
            i18n=None,
        )
        self.assertEqual(rctx.stream_url("pk:recordings:a", 3), "/ether/v1/kind-media/remote-part")
        self.assertEqual(rctx.stream_url("pk:recordings:a", 1, source="epub", fallback=True), "/ether/v1/kind-media/remote-part")
        self.assertEqual(
            seen,
            [
                ("POST", "/ether/plugin-host/frontend/stream-url", {"viewerId": 7, "itemRef": "pk:recordings:a", "partIndex": 3}),
                ("POST", "/ether/plugin-host/frontend/stream-url", {"viewerId": 7, "itemRef": "pk:recordings:a", "partIndex": 1, "source": "epub", "fallback": True}),
            ],
        )

        # No viewer header: refused.
        ctx.headers = {}
        with self.assertRaises(RuntimeError):
            ctx.stream_url("pk:recordings:a")




class ArchiveExtractSdkTest(unittest.TestCase):
    def test_mock_and_remote_archive_extract(self):
        import hashlib
        import io
        import zipfile
        import zlib
        from ether_plugin import RemoteHostClient

        buf = io.BytesIO()
        payload = b"panasonic-3do-track-data"
        with zipfile.ZipFile(buf, "w", compression=zipfile.ZIP_DEFLATED) as zf:
            zf.writestr("disc/track01.iso", payload)
            zf.writestr("../unsafe.iso", b"bad")

        mock = create_mock_host_client()
        archive_ref = mock.cache.put("pk-test-games", "disc.zip", buf.getvalue())
        members = mock.archive.list(archive_ref)
        self.assertEqual(len(members), 2)
        self.assertTrue(members[1].get("unsafe"))

        extracted = mock.archive.extract(archive_ref, 0)
        self.assertEqual(extracted["ref"], "blob:" + hashlib.sha256(payload).hexdigest())
        self.assertEqual(extracted["size"], len(payload))
        self.assertEqual(extracted["sha1"], hashlib.sha1(payload).hexdigest())
        self.assertEqual(extracted["md5"], hashlib.md5(payload).hexdigest())
        self.assertEqual(extracted["crc32"], f"{zlib.crc32(payload) & 0xFFFFFFFF:08x}")
        self.assertEqual(mock.cache.open(extracted["ref"]), payload)

        unsafe_extracted = mock.archive.extract(archive_ref, 1)
        self.assertEqual(mock.cache.open(unsafe_extracted["ref"]), b"bad")

        seen = []
        client = RemoteHostClient(base_url="http://ether.local", credential="cred")
        client.archive._call = lambda method, path, body=None, allow_404=False: (  # type: ignore[method-assign]
            seen.append((method, path, body)) or extracted
        )
        out = client.archive.extract(archive_ref, 0)
        self.assertEqual(out, extracted)
        self.assertEqual(seen, [("POST", "/ether/plugin-host/data/archive.extract", [archive_ref, 0])])

    def test_mock_and_remote_archive_read_and_runtime_limits(self):
        import base64
        import io
        import zipfile
        from ether_plugin import RemoteHostClient

        buf = io.BytesIO()
        with zipfile.ZipFile(buf, "w", compression=zipfile.ZIP_DEFLATED) as zf:
            zf.writestr("sets/a.xml", b"alpha-xml")
            zf.writestr("sets/b.xml", b"bravo-xml")
            zf.writestr("../bad.xml", b"unsafe")

        mock = create_mock_host_client()
        self.assertEqual(mock.runtime.limits()["readBytes"], 8 * 1024 * 1024)
        archive_ref = mock.cache.put("pk-test-games", "sets.zip", buf.getvalue())
        batch = mock.archive.read(
            archive_ref,
            [
                {"member": 1, "offset": 0, "length": 5},
                {"member": 0, "offset": 6, "length": 3},
            ],
        )
        self.assertEqual(
            batch,
            [
                {"index": 0, "member": 1, "size": 5, "body": b"bravo"},
                {"index": 1, "member": 0, "size": 3, "body": b"xml"},
            ],
        )
        with self.assertRaises(ValueError):
            mock.archive.read(archive_ref, [])
        with self.assertRaises(ValueError):
            mock.archive.read(
                archive_ref,
                [
                    {"member": 0, "offset": 0, "length": 5 * 1024 * 1024},
                    {"member": 1, "offset": 0, "length": 4 * 1024 * 1024},
                ],
            )
        flagged = mock.archive.read(archive_ref, [{"member": 2, "offset": 0, "length": 6}])
        self.assertEqual(flagged, [{"index": 0, "member": 2, "size": 6, "body": b"unsafe"}])

        seen = []
        client = RemoteHostClient(base_url="http://ether.local", credential="cred")

        def fake_call(method, path, body=None, allow_404=False):
            seen.append((method, path, body))
            if path == "/ether/plugin-host/limits":
                return {"memoryBytes": 1 << 30, "readBytes": 256 << 20}
            return [
                {
                    "index": 0,
                    "member": 1,
                    "size": 5,
                    "bodyBase64": base64.b64encode(b"bravo").decode("ascii"),
                }
            ]

        client.runtime._call = fake_call  # type: ignore[method-assign]
        client.archive._call = fake_call  # type: ignore[method-assign]
        self.assertEqual(client.runtime.limits()["readBytes"], 256 << 20)
        remote_out = client.archive.read(archive_ref, [{"member": 1, "offset": 0, "length": 5}])
        self.assertEqual(remote_out, [{"index": 0, "member": 1, "size": 5, "body": b"bravo"}])
        self.assertEqual(seen[0], ("GET", "/ether/plugin-host/limits", None))
        self.assertEqual(seen[1][1], "/ether/plugin-host/data/archive.read")

    def test_kinds_visible_and_browse(self):
        from ether_plugin import (
            HEADER_VIEWER_TOKEN,
            MAX_KIND_VISIBLE_IDS,
            RemoteHostClient,
            kinds_browse,
            kinds_visible,
        )

        mock = create_mock_host_client()
        mock.kinds.upsert(
            "comic",
            [
                {"id": "a", "level": "series", "title": "Alpha"},
                {"id": "b", "level": "series", "title": "Beta"},
                {"id": "c", "level": "series", "title": "Gamma"},
            ],
        )
        mock.kind_hidden_ids["comic"] = {"b"}

        with self.assertRaises(RuntimeError):
            mock.kinds.visible("comic", ["a", "b"])
        with self.assertRaises(RuntimeError):
            mock.kinds.browse("comic", {})
        with self.assertRaises(ValueError):
            mock.kinds.visible("comic", [f"id-{i}" for i in range(MAX_KIND_VISIBLE_IDS + 1)], viewer_token="tok-1")

        self.assertEqual(mock.kinds.visible("comic", ["c", "b", "missing", "a", "c"], viewer_token="tok-1"), ["c", "a"])
        page = mock.kinds.browse("comic", {"query": "a", "limit": 10}, viewer_token="tok-1")
        self.assertEqual([it["id"] for it in page["items"]], ["a", "c"])

        # FrontendAdapter 5-arg handle receives ctx with X-Ether-Viewer-Token.
        app = plugin(
            manifest={
                "id": "org.example.comics",
                "name": "Comics",
                "version": "1.0.0",
                "ether": {"protocol": 1, "kind": "content"},
            },
            routes={},
            host=mock,
        )

        class Adapter:
            def handle(self, method, path, headers, body, ctx):
                ids = kinds_visible(ctx, "comic", ["a", "b", "c"])
                bpage = kinds_browse(ctx, "comic", {"desc": True, "limit": 10})
                return (
                    200,
                    {"Content-Type": "application/json"},
                    json.dumps({"ids": ids, "browse": [it["id"] for it in bpage["items"]]}),
                )

        app.on_frontend("opds", Adapter())
        res = app.dispatch(
            {
                "method": "GET",
                "path": "/ether/v1/frontend/opds/feed",
                "headers": {HEADER_VIEWER_TOKEN: "tok-live"},
            }
        )
        self.assertEqual(res["status"], 200)
        self.assertEqual(json.loads(res["body"]), {"ids": ["a", "c"], "browse": ["c", "a"]})

        no_tok = app.dispatch(
            {
                "method": "GET",
                "path": "/ether/v1/frontend/opds/feed",
                "headers": {"X-Ether-Viewer": "42"},
            }
        )
        self.assertEqual(no_tok["status"], 500)

        # RemoteHostClient sends X-Ether-Viewer-Token header and never viewerId in body.
        seen = []
        client = RemoteHostClient(base_url="http://ether.local", credential="cred")

        def fake_call(method, path, body=None, allow_404=False, extra_headers=None):
            seen.append((method, path, body, extra_headers))
            if path.endswith("/visible"):
                return {"ids": ["a"]}
            return {"items": [{"id": "a", "level": "series", "title": "Alpha"}]}

        client.kinds._call = fake_call  # type: ignore[method-assign]
        with self.assertRaises(RuntimeError):
            client.kinds.visible("comic", ["a"])
        with self.assertRaises(RuntimeError):
            client.kinds.browse("comic", {})
        self.assertEqual(client.kinds.visible("comic", ["a", "b"], viewer_token="tok-r"), ["a"])
        r_page = client.kinds.browse("comic", {"query": "alp", "desc": True}, viewer_token="tok-r")
        self.assertEqual(len(r_page["items"]), 1)
        self.assertEqual(seen[0], ("POST", "/ether/plugin-host/kinds/comic/visible", {"ids": ["a", "b"]}, {HEADER_VIEWER_TOKEN: "tok-r"}))
        self.assertNotIn("viewerId", seen[0][2])
        self.assertEqual(seen[1], ("POST", "/ether/plugin-host/kinds/comic/browse", {"query": "alp", "desc": True}, {HEADER_VIEWER_TOKEN: "tok-r"}))
        self.assertNotIn("viewerId", seen[1][2])

    def test_kind_live_dispatch(self):
        app = plugin(
            manifest={
                "id": "org.example.games",
                "name": "Games",
                "version": "1.0.0",
                "ether": {"protocol": 1, "kind": "content"},
            },
            routes={},
        )
        seen = {}

        class LiveProv:
            def live(self, ctx, req):
                seen["live"] = req
                return {
                    "items": [{"id": "ch-1", "title": "Finals", "badge": "LIVE", "viewers": 420}],
                    "filters": [{"key": "featured", "label": "Featured"}],
                    "nextCursor": "cur-2",
                }

            def resolve_live(self, ctx, req):
                seen["resolve"] = req
                return {
                    "url": "https://stream.example/live/ch-1/master.m3u8?token=once",
                    "mime": "application/vnd.apple.mpegurl",
                    "title": "Finals",
                }

        app.on_live("esports", LiveProv())
        res = app.dispatch(
            {
                "method": "POST",
                "path": "/ether/v1/kinds/esports/live",
                "body": json.dumps({"tab": "live", "q": "finals", "filter": "featured", "limit": 10}),
            }
        )
        self.assertEqual(res["status"], 200)
        self.assertEqual(seen["live"]["kind"], "esports")
        self.assertEqual(seen["live"]["q"], "finals")
        self.assertEqual(res["body"]["nextCursor"], "cur-2")

        res2 = app.dispatch(
            {
                "method": "POST",
                "path": "/ether/v1/kinds/esports/live/resolve",
                "body": json.dumps({"id": "ch-1"}),
            }
        )
        self.assertEqual(res2["status"], 200)
        self.assertEqual(seen["resolve"], {"kind": "esports", "id": "ch-1"})
        self.assertEqual(res2["body"]["url"], "https://stream.example/live/ch-1/master.m3u8?token=once")


if __name__ == "__main__":
    unittest.main()
