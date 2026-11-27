package wasm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/fastschema/qjs"
)

// faultPrefix marks a classified fault inside a JS exception message, the
// same trick appkit-sandbox uses to carry faults across the boundary.
const faultPrefix = "__ETHER_FAULT__"

func encodeFault(f *Fault) string {
	b, _ := json.Marshal(f)
	return faultPrefix + string(b)
}

func decodeFault(msg string) *Fault {
	at := strings.Index(msg, faultPrefix)
	if at < 0 {
		return nil
	}
	var f Fault
	// The payload may be followed by a JS stack trace; decode one value.
	dec := json.NewDecoder(strings.NewReader(msg[at+len(faultPrefix):]))
	if dec.Decode(&f) != nil || f.Code == "" {
		return nil
	}
	return &f
}

var qjsOnce sync.Once

// ensureQJS creates the first QuickJS runtime in the process with
// CloseOnContextDone enabled. fastschema/qjs caches its wazero runtime
// config package-globally from the first New call, and context-driven
// module close is the only reliable way to interrupt a tight JS loop
// (its MaxExecutionTime does not interrupt one), so the first runtime
// must be ours.
func ensureQJS() {
	qjsOnce.Do(func() {
		defer func() { _ = recover() }()
		if rt, err := qjs.New(qjs.Option{Context: context.Background(), CloseOnContextDone: true,
			Stdout: io.Discard, Stderr: io.Discard}); err == nil {
			defer func() { _ = recover() }()
			rt.Close()
		}
	})
}

// codecPrelude installs __ether_text_decode, __ether_text_encode, TextDecoder,
// and TextEncoder backed by native Go UTF-8 helpers so guest plugins never pay
// per-byte JavaScript loops when decoding or encoding UTF-8 buffers.
const codecPrelude = `
"use strict";
(() => {
  const rawDecode = __ether_raw_text_decode;
  const rawEncode = __ether_raw_text_encode;
  delete globalThis.__ether_raw_text_decode;
  delete globalThis.__ether_raw_text_encode;

  const utf8Labels = new Set([
    "utf-8",
    "utf8",
    "unicode-1-1-utf-8",
    "unicode11utf8",
    "unicode20utf8",
    "x-unicode20utf8",
  ]);

  const toView = (input, fnName) => {
    if (ArrayBuffer.isView(input)) {
      return new Uint8Array(input.buffer, input.byteOffset, input.byteLength);
    }
    if (input instanceof ArrayBuffer) {
      return new Uint8Array(input);
    }
    throw new TypeError(fnName + ": input must be an ArrayBuffer or ArrayBufferView");
  };

  const decodeBuf = (ab, offset, length, fatal) => {
    try {
      return rawDecode(ab, offset, length, fatal);
    } catch (e) {
      const msg = String(e && e.message || e);
      if (fatal && msg.indexOf("not valid for encoding") >= 0) {
        throw new TypeError(msg);
      }
      if (msg.indexOf("detached") >= 0 || msg.indexOf("ArrayBuffer") >= 0) {
        throw new TypeError(msg);
      }
      throw e;
    }
  };

  globalThis.__ether_text_decode = (input) => {
    if (input === undefined) return "";
    if (ArrayBuffer.isView(input)) {
      return decodeBuf(input.buffer, input.byteOffset, input.byteLength, false);
    }
    if (input instanceof ArrayBuffer) {
      return decodeBuf(input, 0, input.byteLength, false);
    }
    throw new TypeError("__ether_text_decode: input must be an ArrayBuffer or ArrayBufferView");
  };

  globalThis.__ether_text_encode = (input) => {
    const s = input === undefined ? "" : String(input);
    return s.indexOf("\x00") < 0 ? rawEncode(s, false) : rawEncode(JSON.stringify(s), true);
  };

  const validSecondByte = (lead, b1) => {
    if (lead === 0xe0) return b1 >= 0xa0 && b1 <= 0xbf;
    if (lead === 0xed) return b1 >= 0x80 && b1 <= 0x9f;
    if (lead === 0xf0) return b1 >= 0x90 && b1 <= 0xbf;
    if (lead === 0xf4) return b1 >= 0x80 && b1 <= 0x8f;
    return b1 >= 0x80 && b1 <= 0xbf;
  };

  const trailingIncomplete = (u8) => {
    const len = u8.byteLength;
    if (len === 0) return 0;
    const b0 = u8[len - 1];
    if (b0 >= 0xc2 && b0 <= 0xf4) return 1;
    if (len >= 2 && b0 >= 0x80 && b0 <= 0xbf) {
      const p1 = u8[len - 2];
      if (p1 >= 0xe0 && p1 <= 0xf4 && validSecondByte(p1, b0)) return 2;
      if (len >= 3) {
        const p2 = u8[len - 3];
        if (p2 >= 0xf0 && p2 <= 0xf4 && validSecondByte(p2, p1)) return 3;
      }
    }
    return 0;
  };

  if (typeof globalThis.TextDecoder !== "function") {
    class TextDecoder {
      constructor(label = "utf-8", options = undefined) {
        const norm = String(label).replace(/^[\t\n\f\r ]+|[\t\n\f\r ]+$/g, "").toLowerCase();
        if (!utf8Labels.has(norm)) {
          throw new RangeError("TextDecoder: unsupported encoding \"" + label + "\"");
        }
        if (options !== undefined && options !== null && typeof options !== "object") {
          throw new TypeError("TextDecoder: options must be an object");
        }
        this._fatal = Boolean(options && options.fatal);
        this._ignoreBOM = Boolean(options && options.ignoreBOM);
        this._pending = null;
        this._bomSeen = false;
      }
      get encoding() { return "utf-8"; }
      get fatal() { return this._fatal; }
      get ignoreBOM() { return this._ignoreBOM; }
      decode(input = undefined, options = undefined) {
        if (options !== undefined && options !== null && typeof options !== "object") {
          throw new TypeError("TextDecoder.decode: options must be an object");
        }
        const stream = Boolean(options && options.stream);
        if (!stream && !this._pending) {
          const hadBomSeen = this._bomSeen;
          this._bomSeen = false;
          if (input === undefined) return "";
          let out;
          if (ArrayBuffer.isView(input)) {
            out = decodeBuf(input.buffer, input.byteOffset, input.byteLength, this._fatal);
          } else if (input instanceof ArrayBuffer) {
            out = decodeBuf(input, 0, input.byteLength, this._fatal);
          } else {
            throw new TypeError("TextDecoder.decode: input must be an ArrayBuffer or ArrayBufferView");
          }
          if (!this._ignoreBOM && !hadBomSeen && out.length > 0 && out.charCodeAt(0) === 0xfeff) {
            return out.slice(1);
          }
          return out;
        }
        const view = input === undefined ? new Uint8Array(0) : toView(input, "TextDecoder.decode");
        if (input !== undefined && view.byteLength === 0) {
          decodeBuf(view.buffer, view.byteOffset, 0, this._fatal);
        }
        let combined = view;
        if (this._pending) {
          combined = new Uint8Array(this._pending.length + view.byteLength);
          combined.set(this._pending, 0);
          combined.set(view, this._pending.length);
          this._pending = null;
        }
        let decodeLen = combined.byteLength;
        if (stream && decodeLen > 0) {
          const keep = trailingIncomplete(combined);
          if (keep > 0) {
            decodeLen -= keep;
            this._pending = combined.slice(decodeLen);
          }
        }
        const rawOut = decodeLen > 0
          ? decodeBuf(combined.buffer, combined.byteOffset, decodeLen, this._fatal)
          : "";
        let res = rawOut;
        if (rawOut.length > 0) {
          if (!this._ignoreBOM && !this._bomSeen && rawOut.charCodeAt(0) === 0xfeff) {
            res = rawOut.slice(1);
          }
          this._bomSeen = true;
        }
        if (!stream) {
          this._bomSeen = false;
        }
        return res;
      }
    }
    globalThis.TextDecoder = TextDecoder;
  }

  if (typeof globalThis.TextEncoder !== "function") {
    class TextEncoder {
      get encoding() { return "utf-8"; }
      encode(input = "") {
        return new Uint8Array(globalThis.__ether_text_encode(input === undefined ? "" : String(input)));
      }
    }
    globalThis.TextEncoder = TextEncoder;
  }
})();
`

// servicePrelude builds the ether global for request-ABI instances. Host
// calls are synchronous functions backed by __ether_host, charged by the
// Go side; a Fault comes back as a thrown Error whose message carries the
// fault prefix, and ether.abort throws an aborted fault.
const servicePrelude = `
"use strict";
globalThis.ether = (() => {
  const call = (name, args) => {
    try { return JSON.parse(__ether_host(name, JSON.stringify(args))); }
    catch (e) {
      const message = String(e && e.message || e);
      const at = message.indexOf("__ETHER_FAULT__");
      if (at >= 0) {
        try {
          const fault = JSON.parse(message.slice(at + "__ETHER_FAULT__".length));
          e.code = fault.code || fault.Code;
          if (typeof fault.retryAfterSec === "number") e.retryAfterSec = fault.retryAfterSec;
          if (typeof fault.retryAt === "string") e.retryAt = fault.retryAt;
        } catch (_) {}
      }
      throw e;
    }
  };
  const ether = {
    log: (...a) => { try { __ether_host("log", JSON.stringify(a.map(String))); } catch (e) {} },
    abort: (msg) => { throw new Error("__ETHER_FAULT__" + JSON.stringify({ code: "aborted", message: String(msg == null ? "aborted" : msg) })); },
    runtime: { limits: () => call("runtime.limits", []) },
    kv: { get: (k) => call("kv.get", [k]), put: (k, v) => call("kv.put", [k, v]),
          delete: (k) => call("kv.delete", [k]), list: (p) => call("kv.list", [p == null ? "" : p]),
          usage: () => call("kv.usage", []) },
    badges: { set: (setKey, entries) => call("badges.set", [setKey, entries == null ? [] : entries]),
              clear: (setKey) => call("badges.clear", [setKey]),
              usage: () => call("badges.usage", []) },
    settings: { get: () => call("settings.get", []) },
    secret: { get: (n) => call("secret.get", [n]),
              set: (n, v, ttlSec) => call("secret.set", ttlSec === undefined ? [n, v] : [n, v, ttlSec]),
              delete: (n) => call("secret.delete", [n]) },
    http: { hash: (req) => call("http.hash", [req]),
            fetch: (req) => call("http.fetch", [req]),
            fetchMany: (reqs, opts) => call("http.fetchMany", opts == null ? [reqs == null ? [] : reqs] : [reqs == null ? [] : reqs, opts]),
            fetchToCache: (req) => call("http.fetchToCache", [req]) },
    catalog: { search: (q) => call("catalog.search", [q]), get: (id) => call("catalog.get", [id]) },
    addon: { get: (route) => call("addon.get", [route]) },
    viewers: { granted: () => call("viewers.granted", []), history: (id, page) => call("viewers.history", [id, page == null ? 0 : page]),
               setWatchState: (v, titleId, watched) => call("viewers.setWatchState",
                 typeof v === "object" && v !== null ? [v.viewerId, v.titleId, v.watched] : [v, titleId, watched]) },
    collections: { upsert: (c) => call("collections.upsert", [c]) },
    screens: { invalidate: () => call("screens.invalidate", []),
               putRecords: (records) => call("screens.putRecords", [records == null ? [] : records]) },
    kinds: { upsert: (kind, items) => call("kinds.upsert", [kind, items == null ? [] : items]),
             delete: (kind, ids) => call("kinds.delete", [kind, ids == null ? [] : ids]),
             items: (kind, cursor, limit) => call("kinds.items", [kind, cursor == null ? "" : cursor, limit == null ? 0 : limit]),
             visible: (kind, ids) => call("kinds.visible", [kind, ids == null ? [] : ids]),
             browse: (kind, query) => call("kinds.browse", [kind, query == null ? {} : query]),
             expand: (kind, id) => call("kinds.expand", [kind, id]),
             search: (kind, query, opts) => call("kinds.search", [kind, query, opts == null ? {} : opts]),
             get: (kind, ids) => call("kinds.get", [kind, ids == null ? [] : ids]),
             state: (kind, ids) => call("kinds.state", [kind, ids == null ? [] : ids]),
             favorite: (kind, ids, on) => call("kinds.favorite", [kind, ids == null ? [] : ids, on !== false]),
             played: (kind, id) => call("kinds.played", [kind, id]),
             rate: (kind, id, rating) => call("kinds.rate", [kind, id, rating == null ? 0 : rating]),
             favorites: (kind, query) => call("kinds.favorites", [kind, query == null ? {} : query]),
             playlists: (kind) => call("kinds.playlists", [kind]),
             playlist: (kind, id) => call("kinds.playlist", [kind, id]),
             playlistCreate: (kind, title, ids) => call("kinds.playlistCreate", [kind, title, ids == null ? [] : ids]),
             playlistUpdate: (kind, id, update) => call("kinds.playlistUpdate", [kind, id, update == null ? {} : update]),
             playlistDelete: (kind, id) => call("kinds.playlistDelete", [kind, id]) },
    sources: { resolve: (q) => call("sources.resolve", [q == null ? {} : q]) },
    cache: { put: (lib, name, data, opts) => call("cache.put", [lib, name ?? "", data ?? null, opts ?? null]),
             open: (blobRef) => call("cache.open", [blobRef]), read: (blobRef, range) => call("cache.read", [blobRef, range]),
             stat: (blobRef) => call("cache.stat", [blobRef]),
             rasterize: (lib, name, blobRef, opts) => call("cache.rasterize", opts == null ? [lib, name, blobRef] : [lib, name, blobRef, opts]),
             delete: (blobRef) => call("cache.delete", [blobRef]) },
    archive: { list: (blobRef) => call("archive.list", [blobRef]), open: (blobRef, member, range) => call("archive.open", [blobRef, member, range]), read: (blobRef, reads) => call("archive.read", [blobRef, reads]), extract: (blobRef, member, opts) => call("archive.extract", opts == null ? [blobRef, member] : [blobRef, member, opts]) },
    frontend: { streamURL: (itemRef, opts) => call("frontend.streamURL", opts == null ? [itemRef] : [itemRef, opts]) },
    folders: { list: (root, path, options) => call("folders.list", [root, path ?? "", options ?? {}]),
               hashStart: (root, path, options) => call("folders.hashStart", [root, path, options ?? {}]),
               hashPoll: (id) => call("folders.hashPoll", [id]),
               hashCancel: (id) => call("folders.hashCancel", [id]),
               hash: (root, path, options) => call("folders.hash", [root, path, options ?? {}]),
               frame: (root, path, options) => call("folders.frame", [root, path, options]),
               stat: (root, path) => call("folders.stat", [root, path]) },
    events: { publish: (key, payload, correlation) => call("events.publish", correlation == null ? [key, payload == null ? {} : payload] : [key, payload == null ? {} : payload, correlation]) },
    ai: { complete: (req) => call("ai.complete", [req == null ? {} : req]) },
    media: { probe: (source) => call("media.probe", [source == null ? {} : source]) },
    notify: { owner: (text) => call("notify.owner", [text]) },
  };
  return ether;
})();
globalThis.console = { log: (...a) => ether.log(...a), error: (...a) => ether.log(...a), warn: (...a) => ether.log(...a) };
`

// hotPrelude locks a hot-path instance down: no host services except log
// and abort, Date fixed at the epoch, Math.random a seeded generator that
// __ether_rank_batch resets per batch. The monotonic clock is captured in
// a closure for budget checks, then hidden from plugin code.
//
// Budgets are measured with performance.now (WASI's monotonic clock), not
// Date.now: the wall clock counts whole milliseconds, can step, and on
// Windows advances only once per ~15.6 ms timer tick, so a 2 ms budget read
// from it flagged fast candidates that straddled a tick as slow. wazero
// backs the monotonic clock with QueryPerformanceCounter on Windows.
const hotPrelude = `
"use strict";
globalThis.ether = (() => {
  // Capture the monotonic clock for budget checks before it is replaced.
  // It lives only in this closure; plugin code sees the frozen Date and
  // performance below, and a host-function clock would cost a boundary
  // crossing per check.
  const now = performance.now.bind(performance);
  // The calling thread's CPU time in ms, or -1 without a CPU clock. Hidden
  // from plugin code like the monotonic clock.
  const cpu = typeof __ether_cpu_ms === "function" ? __ether_cpu_ms : () => -1;
  delete globalThis.__ether_cpu_ms;
  let state = 0x2545F491;
  const nextRandom = () => { state = (state * 1103515245 + 12345) & 0x7fffffff; return state / 0x80000000; };
  class FixedDate extends Date { constructor(...a) { super(...(a.length ? a : [0])); } static now() { return 0; } }
  globalThis.Date = FixedDate;
  globalThis.performance = Object.freeze({ now: () => 0, timeOrigin: 0 });
  Math.random = nextRandom;
  // A batch arrives in two calls. __ether_load_batch parses the facts and
  // the config or condition value: host work, run with the JSON functions
  // captured here so no plugin code runs in it, under its own deadline.
  // __ether_rank_batch and __ether_cond_batch then run the plugin over it.
  //
  // Budgets bound the plugin's own time (SPEC 5.2): each candidate gets
  // perMs, and the batch stops starting candidates once batchMs is spent;
  // the rest come back null (neutral, slow, not memoized). Wall time on a
  // busy host includes the time the thread sat descheduled, which is not
  // the plugin's. So the cheap monotonic clock is read first, and when it
  // says a budget is spent the thread's CPU clock decides: the batch goes on
  // while the plugin's CPU time is within budget, and a candidate over
  // budget by the wall clock runs once more and is slow only if that run's
  // CPU time is over budget too. Without a CPU clock the wall clock rules.
  const parse = JSON.parse, stringify = JSON.stringify;
  let batch = [], batchArg = null;
  globalThis.__ether_load_batch = (candsJson, argJson) => {
    batch = parse(candsJson);
    batchArg = parse(argJson);
    return batch.length;
  };
  const each = (perMs, batchMs, call, accept) => {
    const cands = batch;
    batch = [];
    const out = [];
    for (let i = 0; i < cands.length; i++) out.push(null);
    const wall0 = now(), cpu0 = cpu();
    for (let i = 0; i < cands.length; i++) {
      if (now() - wall0 > batchMs && (cpu0 < 0 || cpu() - cpu0 > batchMs)) break;
      let t0 = now(), v = call(cands[i]);
      if (now() - t0 > perMs) {
        // Over by the wall clock: run it again and judge the plugin's own time.
        const c0 = cpu();
        t0 = now();
        v = call(cands[i]);
        if (c0 < 0 ? now() - t0 > perMs : cpu() - c0 > perMs) continue;
      }
      out[i] = accept(v);
    }
    return stringify(out);
  };
  const score = (v) => (typeof v === "bigint" ? Number(v) : v);
  globalThis.__ether_rank_batch = (perMs, batchMs) => {
    state = 0x2545F491;
    const cfg = batchArg;
    return each(perMs, batchMs, (c) => rank_key(c, cfg), score);
  };
  globalThis.__ether_cond_batch = (op, perMs, batchMs) => {
    state = 0x2545F491;
    const value = batchArg;
    return each(perMs, batchMs, (c) => cond_match(c, op, value), (v) => !!v);
  };
  return {
    log: (...a) => { try { __ether_host("log", JSON.stringify(a.map(String))); } catch (e) {} },
    abort: (msg) => { throw new Error("__ETHER_FAULT__" + JSON.stringify({ code: "aborted", message: String(msg == null ? "aborted" : msg) })); },
  };
})();
globalThis.console = { log: (...a) => ether.log(...a), error: (...a) => ether.log(...a), warn: (...a) => ether.log(...a) };
`

// exportProbe reports entry points and normalizes esbuild's IIFE exports
// (global name __plugin) onto globalThis so both authoring styles work.
const exportProbe = `
(() => {
  const pick = (n) => (typeof globalThis[n] === "function" ? globalThis[n]
    : (globalThis.__plugin && typeof globalThis.__plugin[n] === "function" ? globalThis.__plugin[n] : undefined));
  for (const n of ["handle", "rank_key", "cond_match"]) { const f = pick(n); if (f) globalThis[n] = f; }
  return JSON.stringify({ handle: typeof globalThis.handle === "function",
    rank_key: typeof globalThis.rank_key === "function",
    cond_match: typeof globalThis.cond_match === "function" });
})()
`

// swapCtx is a context whose target can be swapped atomically per call.
// qjs calls wasm functions with its stored Context value; giving it a
// swapCtx lets one pooled instance take per-call deadlines (which close
// the module on expiry) without racing wazero's cancellation watcher.
type swapCtx struct {
	cur atomic.Pointer[context.Context]
}

func newSwapCtx() *swapCtx {
	s := &swapCtx{}
	bg := context.Background()
	s.cur.Store(&bg)
	return s
}

func (s *swapCtx) set(ctx context.Context) { s.cur.Store(&ctx) }
func (s *swapCtx) reset()                  { s.set(context.Background()) }
func (s *swapCtx) Deadline() (time.Time, bool) {
	return (*s.cur.Load()).Deadline()
}
func (s *swapCtx) Done() <-chan struct{} { return (*s.cur.Load()).Done() }
func (s *swapCtx) Err() error            { return (*s.cur.Load()).Err() }
func (s *swapCtx) Value(key any) any     { return (*s.cur.Load()).Value(key) }

// jsLogWriter keeps WASI output out of the process streams.
type jsLogWriter struct {
	logger   *slog.Logger
	pluginID string
}

func (w jsLogWriter) Write(p []byte) (int, error) {
	if w.logger != nil {
		w.logger.Debug("plugin output", "plugin", w.pluginID, "output", string(p))
	}
	return len(p), nil
}

type jsInstance struct {
	rt           *qjs.Runtime
	qc           *qjs.Context
	swap         *swapCtx
	instanceCaps caps
	hot          bool
	cs           *callState // set for the duration of one call
}

func newJSArrayBuffer(qc *qjs.Context, data []byte) (*qjs.Value, error) {
	if len(data) == 0 {
		v := qc.Call("QJS_NewArrayBufferCopy", qc.Raw(), 0, 0)
		if qc.HasException() {
			return nil, qc.Exception()
		}
		return v, nil
	}
	ptr := qc.Malloc(uint64(len(data)))
	if ptr == 0 {
		return nil, errors.New("out of guest memory")
	}
	defer qc.FreeHandle(ptr)
	qc.MemWrite(uint32(ptr), data)
	v := qc.Call("QJS_NewArrayBufferCopy", qc.Raw(), ptr, uint64(len(data)))
	if qc.HasException() {
		return nil, qc.Exception()
	}
	return v, nil
}

// whatwgDecodeUTF8 implements the WHATWG Encoding Standard UTF-8 decoder,
// replacing each maximal ill-formed byte subpart with U+FFFD (or returning an
// error when fatal is true). Valid UTF-8 returns b without allocating.
func whatwgDecodeUTF8(b []byte, fatal bool) ([]byte, error) {
	if utf8.Valid(b) {
		return b, nil
	}
	if fatal {
		return nil, errors.New("The encoded data was not valid for encoding utf-8")
	}
	out := make([]byte, 0, len(b)+16)
	var (
		cp            rune
		bytesSeen     int
		bytesNeeded   int
		lowerBoundary byte = 0x80
		upperBoundary byte = 0xBF
	)
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if bytesNeeded == 0 {
			switch {
			case ch <= 0x7F:
				out = append(out, ch)
			case ch >= 0xC2 && ch <= 0xDF:
				bytesNeeded = 1
				cp = rune(ch & 0x1F)
			case ch >= 0xE0 && ch <= 0xEF:
				if ch == 0xE0 {
					lowerBoundary = 0xA0
				} else if ch == 0xED {
					upperBoundary = 0x9F
				}
				bytesNeeded = 2
				cp = rune(ch & 0x0F)
			case ch >= 0xF0 && ch <= 0xF4:
				if ch == 0xF0 {
					lowerBoundary = 0x90
				} else if ch == 0xF4 {
					upperBoundary = 0x8F
				}
				bytesNeeded = 3
				cp = rune(ch & 0x07)
			default:
				out = append(out, 0xEF, 0xBF, 0xBD)
			}
			continue
		}
		if ch < lowerBoundary || ch > upperBoundary {
			cp, bytesNeeded, bytesSeen = 0, 0, 0
			lowerBoundary, upperBoundary = 0x80, 0xBF
			out = append(out, 0xEF, 0xBF, 0xBD)
			i--
			continue
		}
		lowerBoundary, upperBoundary = 0x80, 0xBF
		cp = (cp << 6) | rune(ch&0x3F)
		bytesSeen++
		if bytesSeen == bytesNeeded {
			out = utf8.AppendRune(out, cp)
			cp, bytesNeeded, bytesSeen = 0, 0, 0
		}
	}
	if bytesNeeded != 0 {
		out = append(out, 0xEF, 0xBF, 0xBD)
	}
	return out, nil
}

// wtf8ToUTF8 converts QuickJS's WTF-8 C string output (where lone UTF-16
// surrogates appear as 3-byte 0xED 0xA0..0xBF 0x80..0xBF sequences) into valid
// UTF-8 with one U+FFFD per lone surrogate, matching WHATWG TextEncoder.
func wtf8ToUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] < 0x80 {
			out = append(out, b[i])
			i++
			continue
		}
		r, width := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || width > 1 {
			out = append(out, b[i:i+width]...)
			i += width
			continue
		}
		if i+2 < len(b) && b[i] == 0xED && b[i+1] >= 0xA0 && b[i+1] <= 0xBF && b[i+2] >= 0x80 && b[i+2] <= 0xBF {
			out = append(out, 0xEF, 0xBF, 0xBD)
			i += 3
			continue
		}
		out = append(out, 0xEF, 0xBF, 0xBD)
		i++
	}
	return out
}

// newJSStringUTF8 constructs a QuickJS string from valid UTF-8 bytes without
// leaking the temporary WASM buffer, preserving embedded NUL code units.
func newJSStringUTF8(qc *qjs.Context, b []byte) (*qjs.Value, error) {
	if bytes.IndexByte(b, 0) >= 0 {
		jb, err := json.Marshal(string(b))
		if err != nil {
			return nil, err
		}
		ptr := qc.Malloc(uint64(len(jb) + 1))
		if ptr == 0 {
			return nil, errors.New("out of guest memory")
		}
		defer qc.FreeHandle(ptr)
		dst := qc.MemRead(uint32(ptr), uint64(len(jb)+1))
		copy(dst, jb)
		dst[len(jb)] = 0
		v := qc.Call("QJS_ParseJSON", qc.Raw(), ptr)
		if qc.HasException() {
			return nil, qc.Exception()
		}
		return v, nil
	}
	ptr := qc.Malloc(uint64(len(b) + 1))
	if ptr == 0 {
		return nil, errors.New("out of guest memory")
	}
	defer qc.FreeHandle(ptr)
	dst := qc.MemRead(uint32(ptr), uint64(len(b)+1))
	copy(dst, b)
	dst[len(b)] = 0
	v := qc.Call("QJS_NewString", qc.Raw(), ptr)
	if qc.HasException() {
		return nil, qc.Exception()
	}
	return v, nil
}

func registerTextCodecs(qc *qjs.Context) {
	qc.SetFunc("__ether_raw_text_decode", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		for _, a := range args {
			defer a.Free()
		}
		if len(args) < 3 {
			return nil, errors.New("invalid text decode arguments")
		}
		offset := args[1].Int64()
		length := args[2].Int64()
		fatal := len(args) >= 4 && args[3].Bool()
		packed := qc.Call("QJS_GetArrayBuffer", qc.Raw(), args[0].Raw())
		rawPtr := packed.Raw()
		if rawPtr == 0 {
			if qc.HasException() {
				return nil, qc.Exception()
			}
			if offset != 0 || length != 0 {
				return nil, errors.New("byte range out of bounds")
			}
			return newJSStringUTF8(qc, nil)
		}
		defer qc.FreeHandle(rawPtr)
		hdr := qc.MemRead(uint32(rawPtr), 8)
		pv := binary.LittleEndian.Uint64(hdr)
		bufAddr := uint32(pv >> 32)
		bufSize := int64(uint32(pv))
		if offset < 0 || length < 0 || offset > bufSize || length > bufSize-offset {
			return nil, errors.New("byte range out of bounds")
		}
		if length == 0 {
			return newJSStringUTF8(qc, nil)
		}
		decoded, err := whatwgDecodeUTF8(qc.MemRead(bufAddr+uint32(offset), uint64(length)), fatal)
		if err != nil {
			return nil, err
		}
		return newJSStringUTF8(qc, decoded)
	})
	qc.SetFunc("__ether_raw_text_encode", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		for _, a := range args {
			defer a.Free()
		}
		if len(args) < 1 {
			return newJSArrayBuffer(qc, nil)
		}
		isJSON := len(args) >= 2 && args[1].Bool()
		cstr := qc.Call("QJS_ToCString", qc.Raw(), args[0].Raw())
		rawPtr := cstr.Raw()
		if rawPtr == 0 {
			if qc.HasException() {
				return nil, qc.Exception()
			}
			return newJSArrayBuffer(qc, nil)
		}
		hdr := qc.MemRead(uint32(rawPtr), 8)
		pv := binary.LittleEndian.Uint64(hdr)
		strAddr := uint32(pv >> 32)
		strLen := uint32(pv)
		qc.FreeHandle(rawPtr)
		defer qc.Call("JS_FreeCString", qc.Raw(), uint64(strAddr))
		if strLen == 0 {
			return newJSArrayBuffer(qc, nil)
		}
		rawBytes := qc.MemRead(strAddr, uint64(strLen))
		if isJSON {
			var s string
			if err := json.Unmarshal(rawBytes, &s); err != nil {
				return nil, err
			}
			return newJSArrayBuffer(qc, []byte(s))
		}
		return newJSArrayBuffer(qc, wtf8ToUTF8(rawBytes))
	})
}

func newJSInstance(ctx context.Context, source string, lim *Limits, hot bool, logger *slog.Logger, pluginID string) (in *jsInstance, err error) {
	defer func() {
		if r := recover(); r != nil {
			in, err = nil, &Error{Status: StatusError, Message: fmt.Sprintf("create JS instance: %v", r), Trap: true}
		}
	}()
	swap := newSwapCtx()
	initCtx, cancel := context.WithTimeout(ctx, lim.Deadline)
	defer cancel()
	swap.set(initCtx)
	defer swap.reset()
	rt, err2 := qjs.New(qjs.Option{Context: swap, CloseOnContextDone: true,
		MemoryLimit: int(lim.MemoryBytes), MaxStackSize: 1 << 20,
		Stdout: jsLogWriter{logger, pluginID}, Stderr: jsLogWriter{logger, pluginID}})
	if err2 != nil {
		return nil, &Error{Status: StatusError, Message: "create JS runtime: " + err2.Error(), Trap: true}
	}
	in = &jsInstance{rt: rt, qc: rt.Context(), swap: swap, hot: hot}
	qc := in.qc
	registerTextCodecs(qc)
	qc.SetFunc("__ether_host", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		for _, a := range args {
			defer a.Free()
		}
		if in.cs == nil || len(args) != 2 {
			return nil, errors.New(encodeFault(Forbidden("host call outside a plugin call")))
		}
		out, fault := in.cs.dispatch(args[0].String(), json.RawMessage(args[1].String()))
		if fault != nil {
			return nil, errors.New(encodeFault(fault))
		}
		return qc.NewString(string(out)), nil
	})
	prelude := servicePrelude
	if hot {
		prelude = hotPrelude
		qc.SetFunc("__ether_cpu_ms", func(*qjs.This) (*qjs.Value, error) {
			d, ok := threadCPU()
			if !ok {
				return qc.NewFloat64(-1), nil
			}
			return qc.NewFloat64(budgetMs(d)), nil
		})
	} else {
		// Native base64 -> ArrayBuffer and direct binary host crossings avoid
		// multi-megabyte JSON.parse + base64 copies inside QuickJS on wazero.
		qc.SetFunc("__ether_b64_decode", func(this *qjs.This) (*qjs.Value, error) {
			args := this.Args()
			for _, a := range args {
				defer a.Free()
			}
			if len(args) != 1 {
				return nil, errors.New("Invalid base64 host result")
			}
			decoded, err := base64.StdEncoding.DecodeString(args[0].String())
			if err != nil {
				return nil, errors.New("Invalid base64 host result")
			}
			return newJSArrayBuffer(qc, decoded)
		})
		qc.SetFunc("__ether_host_binary", func(this *qjs.This) (*qjs.Value, error) {
			args := this.Args()
			for _, a := range args {
				defer a.Free()
			}
			if in.cs == nil || len(args) != 2 {
				return nil, errors.New(encodeFault(Forbidden("host call outside a plugin call")))
			}
			name := args[0].String()
			out, fault := in.cs.dispatch(name, json.RawMessage(args[1].String()))
			if fault != nil {
				return nil, errors.New(encodeFault(fault))
			}
			switch name {
			case "archive.open", "cache.read":
				var res struct {
					BodyBase64 string `json:"bodyBase64"`
				}
				if err := json.Unmarshal(out, &res); err != nil {
					return nil, err
				}
				decoded, err := base64.StdEncoding.DecodeString(res.BodyBase64)
				if err != nil {
					return nil, err
				}
				return newJSArrayBuffer(qc, decoded)
			case "archive.read":
				var rows []struct {
					Index      int    `json:"index"`
					Member     int    `json:"member"`
					Size       int    `json:"size"`
					BodyBase64 string `json:"bodyBase64"`
				}
				if err := json.Unmarshal(out, &rows); err != nil {
					return nil, err
				}
				type itemMeta struct {
					Index  int `json:"index"`
					Member int `json:"member"`
					Size   int `json:"size"`
					Offset int `json:"offset"`
				}
				meta := make([]itemMeta, len(rows))
				totalLen := 0
				for _, r := range rows {
					totalLen += base64.StdEncoding.DecodedLen(len(r.BodyBase64))
				}
				concat := make([]byte, totalLen)
				pos := 0
				for i, r := range rows {
					n, err := base64.StdEncoding.Decode(concat[pos:], []byte(r.BodyBase64))
					if err != nil {
						return nil, err
					}
					meta[i] = itemMeta{Index: r.Index, Member: r.Member, Size: n, Offset: pos}
					pos += n
				}
				ab, err := newJSArrayBuffer(qc, concat[:pos])
				if err != nil {
					return nil, err
				}
				metaBytes, err := json.Marshal(meta)
				if err != nil {
					ab.Free()
					return nil, err
				}
				obj := qc.NewObject()
				obj.SetPropertyStr("metaJson", qc.NewString(string(metaBytes)))
				obj.SetPropertyStr("buffer", ab)
				return obj, nil
			default:
				return nil, errors.New("unsupported binary host call")
			}
		})
	}
	if v, err := qc.Eval("ether-codecs.js", qjs.Code(codecPrelude)); err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "codecs prelude: " + err.Error(), Trap: true}
	} else {
		v.Free()
	}
	if v, err := qc.Eval("ether-prelude.js", qjs.Code(prelude)); err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "prelude: " + err.Error(), Trap: true}
	} else {
		v.Free()
	}
	if v, err := qc.Eval("plugin.js", qjs.Code(source)); err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "load plugin: " + firstLine(err.Error()), Trap: false}
	} else {
		v.Free()
	}
	v, err := qc.Eval("ether-probe.js", qjs.Code(exportProbe))
	if err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "probe exports: " + err.Error(), Trap: true}
	}
	probeJSON := v.String()
	v.Free()
	var found struct {
		Handle    bool `json:"handle"`
		RankKey   bool `json:"rank_key"`
		CondMatch bool `json:"cond_match"`
	}
	if err := json.Unmarshal([]byte(probeJSON), &found); err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "probe exports: " + err.Error(), Trap: true}
	}
	in.instanceCaps = caps{Handle: found.Handle, RankKey: found.RankKey, CondMatch: found.CondMatch}
	return in, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (in *jsInstance) caps() caps { return in.instanceCaps }

func (in *jsInstance) closeInstance() {
	defer func() { _ = recover() }()
	in.rt.Close()
}

// jsCall runs fn with the call context installed, converting qjs panics
// (it panics when a deadline closes the module mid-call) into *Error.
func (in *jsInstance) jsCall(ctx context.Context, cs *callState, fn func() (*qjs.Value, error)) (val *qjs.Value, callErr *Error) {
	in.cs = cs
	in.swap.set(ctx)
	defer func() {
		in.cs = nil
		in.swap.reset()
		if r := recover(); r != nil {
			val = nil
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				callErr = &Error{Status: StatusTimeout, Message: cs.timeoutMessage(), Trap: true}
				return
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				callErr = &Error{Status: StatusError, Message: context.Canceled.Error(), Err: context.Canceled, Trap: true}
				return
			}
			callErr = &Error{Status: StatusError, Message: fmt.Sprintf("%v", r), Trap: true}
		}
	}()
	v, err := fn()
	if err != nil {
		return nil, classifyJSError(ctx, cs, err)
	}
	if v != nil && v.IsPromise() {
		v, err = v.Await()
		if err != nil {
			return nil, classifyJSError(ctx, cs, err)
		}
	}
	return v, nil
}

func classifyJSError(ctx context.Context, cs *callState, err error) *Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Status: StatusTimeout, Message: cs.timeoutMessage(), Trap: true}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return &Error{Status: StatusError, Message: context.Canceled.Error(), Err: context.Canceled, Trap: true}
	}
	msg := err.Error()
	if f := decodeFault(msg); f != nil {
		st := statusForFaultCode(f.Code)
		return &Error{Status: st, Message: f.Message, Trap: st == StatusAborted}
	}
	if strings.Contains(msg, "module closed") || strings.Contains(msg, "interrupted") {
		return &Error{Status: StatusTimeout, Message: cs.timeoutMessage(), Trap: true}
	}
	return &Error{Status: StatusError, Message: firstLine(msg)}
}

func (in *jsInstance) globalFn(name string) *qjs.Value {
	return in.qc.Global().GetPropertyStr(name)
}

func (in *jsInstance) handle(ctx context.Context, cs *callState, reqJSON []byte) (out []byte, callErr *Error) {
	in.cs = cs
	in.swap.set(ctx)
	defer func() {
		in.cs = nil
		in.swap.reset()
		if r := recover(); r != nil {
			out = nil
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				callErr = &Error{Status: StatusTimeout, Message: cs.timeoutMessage(), Trap: true}
				return
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				callErr = &Error{Status: StatusError, Message: context.Canceled.Error(), Err: context.Canceled, Trap: true}
				return
			}
			callErr = &Error{Status: StatusError, Message: fmt.Sprintf("%v", r), Trap: true}
		}
	}()
	g := in.qc.Global()
	fn := in.globalFn("__ether_dispatch_handle")
	reqVal := in.qc.NewString(string(reqJSON))
	v, err := in.qc.Invoke(fn, g, reqVal)
	reqVal.Free()
	fn.Free()
	if err != nil {
		return nil, classifyJSError(ctx, cs, err)
	}
	if v != nil && v.IsPromise() {
		v, err = v.Await()
		if err != nil {
			return nil, classifyJSError(ctx, cs, err)
		}
	}
	if v == nil {
		return []byte("{}"), nil
	}
	res := v.String()
	v.Free()
	return []byte(res), nil
}

func (in *jsInstance) rankOne(ctx context.Context, cs *callState, cand, cfg []byte) (int64, *Error) {
	unbounded := Limits{RankPerCandidate: 1 << 60, RankBatch: 1 << 60}
	scores, err := in.rankBatch(ctx, cs, [][]byte{cand}, cfg, &unbounded)
	if err != nil {
		return 0, err
	}
	if scores[0] == nil {
		return 0, &Error{Status: StatusTimeout, Message: "candidate exceeded its budget", Trap: false}
	}
	return *scores[0], nil
}

func budgetMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// loadBatch hands a batch's facts and its config (or condition value) to
// the instance: one parse of one JSON array, under its own deadline, so
// the host's share of a batch is never charged to the plugin.
func (in *jsInstance) loadBatch(ctx context.Context, cs *callState, cands [][]byte, arg []byte, lim *Limits) *Error {
	candsJSON, ferr := factsArray(cands)
	if ferr != nil {
		return ferr
	}
	loadCtx, cancel := context.WithTimeout(ctx, lim.hotKill())
	defer cancel()
	v, err := in.jsCall(loadCtx, cs, func() (*qjs.Value, error) {
		fn := in.globalFn("__ether_load_batch")
		candsVal := in.qc.NewString(string(candsJSON))
		argVal := in.qc.NewString(string(arg))
		res, err := in.qc.Invoke(fn, in.qc.Global(), candsVal, argVal)
		argVal.Free()
		candsVal.Free()
		fn.Free()
		return res, err
	})
	if v != nil {
		v.Free()
	}
	return err
}

func (in *jsInstance) rankBatch(ctx context.Context, cs *callState, cands [][]byte, cfg []byte, lim *Limits) ([]*int64, *Error) {
	if err := in.loadBatch(ctx, cs, cands, cfg, lim); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, lim.hotKill())
	defer cancel()
	runtime.LockOSThread() // the budget reads this thread's CPU clock
	defer runtime.UnlockOSThread()
	v, err := in.jsCall(runCtx, cs, func() (*qjs.Value, error) {
		fn := in.globalFn("__ether_rank_batch")
		perVal := in.qc.NewFloat64(budgetMs(lim.RankPerCandidate))
		batchVal := in.qc.NewFloat64(budgetMs(lim.RankBatch))
		res, err := in.qc.Invoke(fn, in.qc.Global(), perVal, batchVal)
		batchVal.Free()
		perVal.Free()
		fn.Free()
		return res, err
	})
	if err != nil {
		return nil, err
	}
	raw := v.String()
	v.Free()
	var out []*int64
	if uerr := json.Unmarshal([]byte(raw), &out); uerr != nil {
		return nil, &Error{Status: StatusError, Message: "rank_key returned a non-numeric result: " + uerr.Error()}
	}
	if len(out) != len(cands) {
		return nil, &Error{Status: StatusError, Message: "rank batch returned the wrong number of scores"}
	}
	return out, nil
}

// factsArray joins candidate facts into one JSON array, parsed once per
// batch instead of once per candidate. Each fact must be one JSON value, so
// a malformed fact cannot shift the others' positions.
func factsArray(cands [][]byte) ([]byte, *Error) {
	n := 2
	for _, c := range cands {
		if !json.Valid(c) {
			return nil, &Error{Status: StatusError, Message: "candidate facts are not valid JSON"}
		}
		n += len(c) + 1
	}
	b := make([]byte, 0, n)
	b = append(b, '[')
	for i, c := range cands {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, c...)
	}
	return append(b, ']'), nil
}

func (in *jsInstance) condBatch(ctx context.Context, cs *callState, cands [][]byte, op string, val []byte, lim *Limits) ([]*bool, *Error) {
	if err := in.loadBatch(ctx, cs, cands, val, lim); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, lim.hotKill())
	defer cancel()
	runtime.LockOSThread() // the budget reads this thread's CPU clock
	defer runtime.UnlockOSThread()
	v, err := in.jsCall(runCtx, cs, func() (*qjs.Value, error) {
		fn := in.globalFn("__ether_cond_batch")
		opVal := in.qc.NewString(op)
		perVal := in.qc.NewFloat64(budgetMs(lim.RankPerCandidate))
		batchVal := in.qc.NewFloat64(budgetMs(lim.RankBatch))
		res, err := in.qc.Invoke(fn, in.qc.Global(), opVal, perVal, batchVal)
		batchVal.Free()
		perVal.Free()
		opVal.Free()
		fn.Free()
		return res, err
	})
	if err != nil {
		return nil, err
	}
	raw := v.String()
	v.Free()
	var out []*bool
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out) != len(cands) {
		return nil, &Error{Status: StatusError, Message: "invalid condition batch result"}
	}
	return out, nil
}

func (in *jsInstance) condOne(ctx context.Context, cs *callState, cand []byte, op string, val []byte) (bool, *Error) {
	v, err := in.jsCall(ctx, cs, func() (*qjs.Value, error) {
		g := in.qc.Global()
		fn := in.globalFn("cond_match")
		c := in.qc.ParseJSON(string(cand))
		opVal := in.qc.NewString(op)
		value := in.qc.ParseJSON(string(val))
		res, err := in.qc.Invoke(fn, g, c, opVal, value)
		value.Free()
		opVal.Free()
		c.Free()
		fn.Free()
		return res, err
	})
	if err != nil {
		return false, err
	}
	b := v.Bool()
	v.Free()
	return b, nil
}

// LoadJS loads a JavaScript plugin (TypeScript authors go through Compile
// first). A probe instance is built eagerly so syntax errors and a missing
// handle surface at load.
func (e *Engine) LoadJS(id, version, source string, host Host, lim Limits) (*Plugin, error) {
	lim.normalize()
	e.scaleHot(&lim)
	p := &Plugin{id: id, version: version, eng: e, host: host, lim: lim, memo: newMemo(4096), source: source}
	makeSvc := func(ctx context.Context) (instance, error) {
		in, err := newJSInstance(ctx, source, &p.lim, false, e.logger, id)
		if err != nil {
			return nil, err
		}
		if err := in.installDispatch(); err != nil {
			in.closeInstance()
			return nil, err
		}
		return in, nil
	}
	makeHot := func(ctx context.Context) (instance, error) {
		return newJSInstance(ctx, source, &p.lim, true, e.logger, id)
	}
	p.initPools(makeSvc, makeHot)
	probe, err := p.svc.get(context.Background())
	if err != nil {
		p.Close()
		return nil, err
	}
	p.caps = probe.caps()
	p.svc.put(probe, true)
	if !p.caps.Handle && !p.caps.RankKey && !p.caps.CondMatch {
		p.Close()
		return nil, &Error{Status: StatusError, Message: "plugin defines none of handle, rank_key, cond_match"}
	}
	p.warmHot()
	return p, nil
}

// installDispatch wraps handle so Go passes one JSON string and gets one
// JSON string back; the plugin sees plain objects either side.
func (in *jsInstance) installDispatch() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &Error{Status: StatusError, Message: fmt.Sprintf("install dispatch: %v", r), Trap: true}
		}
	}()
	v, err2 := in.qc.Eval("ether-dispatch.js", qjs.Code(`
globalThis.__ether_dispatch_handle = (reqJson) => {
  const res = handle(JSON.parse(reqJson));
  if (res && typeof res.then === "function") return res.then((r) => JSON.stringify(r == null ? {} : r));
  return JSON.stringify(res == null ? {} : res);
};`))
	if err2 != nil {
		return &Error{Status: StatusError, Message: "install dispatch: " + err2.Error(), Trap: true}
	}
	v.Free()
	return nil
}
