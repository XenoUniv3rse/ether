const MAX_WORDMARK_BYTES = 512 * 1024;
const MAX_WIDTH = 1024;
const MAX_HEIGHT = 256;
const MAX_ICON_SIZE = 1024;

export const VELA_SETTINGS_SCHEMA = {
  signInHelp: true,
  sections: [
    {
      key: "appearance",
      fields: [
        { key: "defaultSkinId", kind: "string" },
        { key: "force", kind: "bool" },
        { key: "appName", kind: "string", max: 40 },
        { key: "wordmark", kind: "wordmark" },
        { key: "appIcon", kind: "appIcon" },
      ],
    },
    {
      key: "signIn",
      fields: [
        { key: "passwordSignIn", kind: "bool" },
        { key: "plexSignIn", kind: "bool" },
        { key: "quickConnect", kind: "bool" },
      ],
    },
    {
      key: "trailers",
      fields: [
        { key: "trailerPlayback", kind: "string", options: ["server", "clientWhenPossible"] },
      ],
    },
    {
      key: "liveTV",
      fields: [{ key: "livePreview", kind: "string", options: ["auto", "on-click", "off"] }],
    },
    {
      key: "find",
      fields: [
        { key: "serverName", kind: "serverName" },
        { key: "port", kind: "port", min: 1, max: 65535 },
        { key: "serveOnMainPort", kind: "bool" },
      ],
    },
  ],
};

type StoredAsset = {
  bytes: Uint8Array;
  mime: string;
  width: number;
  height: number;
};

function readUint32BE(bytes: Uint8Array, offset: number): number {
  return (
    ((bytes[offset]! << 24) |
      (bytes[offset + 1]! << 16) |
      (bytes[offset + 2]! << 8) |
      bytes[offset + 3]!) >>>
    0
  );
}

function readUint24LE(bytes: Uint8Array, offset: number): number {
  return bytes[offset]! | (bytes[offset + 1]! << 8) | (bytes[offset + 2]! << 16);
}

function parseWordmarkDimensions(
  bytes: Uint8Array,
): { ext: "png" | "webp" | "svg"; mime: string; width: number; height: number } | null {
  // PNG signature + IHDR
  if (
    bytes.length >= 24 &&
    bytes[0] === 0x89 &&
    bytes[1] === 0x50 &&
    bytes[2] === 0x4e &&
    bytes[3] === 0x47
  ) {
    const width = readUint32BE(bytes, 16);
    const height = readUint32BE(bytes, 20);
    return { ext: "png", mime: "image/png", width, height };
  }
  // RIFF....WEBP
  if (
    bytes.length >= 30 &&
    bytes[0] === 0x52 &&
    bytes[1] === 0x49 &&
    bytes[2] === 0x46 &&
    bytes[3] === 0x46 &&
    bytes[8] === 0x57 &&
    bytes[9] === 0x45 &&
    bytes[10] === 0x42 &&
    bytes[11] === 0x50
  ) {
    const chunk = String.fromCharCode(bytes[12]!, bytes[13]!, bytes[14]!, bytes[15]!);
    if (chunk === "VP8X") {
      const width = readUint24LE(bytes, 24) + 1;
      const height = readUint24LE(bytes, 27) + 1;
      return { ext: "webp", mime: "image/webp", width, height };
    }
    if (chunk === "VP8L" && bytes[20] === 0x2f) {
      const b1 = bytes[21]!;
      const b2 = bytes[22]!;
      const b3 = bytes[23]!;
      const b4 = bytes[24]!;
      const width = (b1 | ((b2 & 0x3f) << 8)) + 1;
      const height = (((b2 & 0xc0) >> 6) | (b3 << 2) | ((b4 & 0x0f) << 10)) + 1;
      return { ext: "webp", mime: "image/webp", width, height };
    }
    if (chunk === "VP8 " && bytes[23] === 0x9d && bytes[24] === 0x01 && bytes[25] === 0x2a) {
      const width = (bytes[26]! | (bytes[27]! << 8)) & 0x3fff;
      const height = (bytes[28]! | (bytes[29]! << 8)) & 0x3fff;
      return { ext: "webp", mime: "image/webp", width, height };
    }
  }
  // SVG
  const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  const svgTag = /<svg\b([^>]*)>/i.exec(text);
  if (svgTag && !/<script\b|<foreignObject\b|\bon[a-z]+\s*=/i.test(text)) {
    const attrs = svgTag[1]!;
    const wMatch = /\bwidth\s*=\s*["']\s*([0-9]+(?:\.[0-9]+)?)(?:px)?\s*["']/i.exec(attrs);
    const hMatch = /\bheight\s*=\s*["']\s*([0-9]+(?:\.[0-9]+)?)(?:px)?\s*["']/i.exec(attrs);
    let width = wMatch ? Math.round(Number(wMatch[1])) : 0;
    let height = hMatch ? Math.round(Number(hMatch[1])) : 0;
    if ((!width || !height) && /\bviewBox\s*=/i.test(attrs)) {
      const vb =
        /\bviewBox\s*=\s*["']\s*[-+0-9.]+\s*[,\s]\s*[-+0-9.]+\s*[,\s]\s*([0-9.]+)\s*[,\s]\s*([0-9.]+)\s*["']/i.exec(
          attrs,
        );
      if (vb) {
        if (!width) width = Math.round(Number(vb[1]));
        if (!height) height = Math.round(Number(vb[2]));
      }
    }
    if (width > 0 && height > 0) {
      return { ext: "svg", mime: "image/svg+xml", width, height };
    }
  }
  return null;
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const buf = new ArrayBuffer(bytes.byteLength);
  new Uint8Array(buf).set(bytes);
  const digest = await crypto.subtle.digest("SHA-256", buf);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
}

export function createVelaBrandingMock() {
  const assets = new Map<string, StoredAsset>();

  async function storeBytes(bytes: Uint8Array) {
    if (bytes.byteLength === 0) {
      return {
        status: 400,
        body: { error: { code: "invalid_wordmark", message: "Wordmark is empty." } },
      };
    }
    if (bytes.byteLength > MAX_WORDMARK_BYTES) {
      return {
        status: 413,
        body: {
          error: { code: "wordmark_too_large", message: "Wordmark must be 512 KB or smaller." },
        },
      };
    }
    let parsed: ReturnType<typeof parseWordmarkDimensions> = null;
    try {
      parsed = parseWordmarkDimensions(bytes);
    } catch {
      parsed = null;
    }
    if (!parsed) {
      return {
        status: 400,
        body: {
          error: {
            code: "invalid_wordmark",
            message: "Wordmark must be a valid PNG, SVG, or WebP image.",
          },
        },
      };
    }
    if (
      parsed.width < 1 ||
      parsed.width > MAX_WIDTH ||
      parsed.height < 1 ||
      parsed.height > MAX_HEIGHT
    ) {
      return {
        status: 400,
        body: {
          error: {
            code: "invalid_wordmark",
            message: "Wordmark dimensions must be between 1×1 and 1024×256 px.",
          },
        },
      };
    }
    const digest = await sha256Hex(bytes);
    const name = `${digest}.${parsed.ext}`;
    assets.set(name, {
      bytes,
      mime: parsed.mime,
      width: parsed.width,
      height: parsed.height,
    });
    return {
      status: 200,
      body: {
        url: `/vela/v0/branding/${name}`,
        width: parsed.width,
        height: parsed.height,
      },
    };
  }

  async function storeIconBytes(bytes: Uint8Array) {
    if (bytes.byteLength === 0) {
      return {
        status: 400,
        body: { error: { code: "invalid_app_icon", message: "App icon is empty." } },
      };
    }
    if (bytes.byteLength > MAX_WORDMARK_BYTES) {
      return {
        status: 413,
        body: {
          error: { code: "too_large", message: "App icon must be 512 KB or smaller." },
        },
      };
    }
    let parsed: ReturnType<typeof parseWordmarkDimensions> = null;
    try {
      parsed = parseWordmarkDimensions(bytes);
    } catch {
      parsed = null;
    }
    if (!parsed) {
      return {
        status: 400,
        body: {
          error: {
            code: "invalid_app_icon",
            message: "App icon must be a valid square PNG, SVG, or WebP image.",
          },
        },
      };
    }
    if (
      parsed.width < 1 ||
      parsed.width > MAX_ICON_SIZE ||
      parsed.height < 1 ||
      parsed.height > MAX_ICON_SIZE ||
      parsed.width !== parsed.height
    ) {
      return {
        status: 400,
        body: {
          error: {
            code: "invalid_app_icon",
            message: "App icon must be square between 1×1 and 1024×1024 px.",
          },
        },
      };
    }
    const digest = await sha256Hex(bytes);
    const name = `icon-${digest}.${parsed.ext}`;
    assets.set(name, {
      bytes,
      mime: parsed.mime,
      width: parsed.width,
      height: parsed.height,
    });
    return {
      status: 200,
      body: {
        url: `/vela/v0/branding/${name}`,
        width: parsed.width,
        height: parsed.height,
        digest,
      },
    };
  }

  return {
    async upload(body: unknown) {
      const file = body instanceof FormData ? body.get("file") : null;
      if (!(file instanceof Blob)) {
        return {
          status: 400,
          body: { error: { code: "invalid_wordmark", message: "Missing wordmark file." } },
        };
      }
      const bytes = new Uint8Array(await file.arrayBuffer());
      return storeBytes(bytes);
    },
    async uploadIcon(body: unknown) {
      const file = body instanceof FormData ? body.get("file") : null;
      if (!(file instanceof Blob)) {
        return {
          status: 400,
          body: { error: { code: "invalid_app_icon", message: "Missing app icon file." } },
        };
      }
      const bytes = new Uint8Array(await file.arrayBuffer());
      return storeIconBytes(bytes);
    },
    serveAsset(asset: string): Response {
      const entry = assets.get(asset);
      if (!entry) return new Response("Not found", { status: 404 });
      const copy = new ArrayBuffer(entry.bytes.byteLength);
      new Uint8Array(copy).set(entry.bytes);
      return new Response(copy, {
        status: 200,
        headers: {
          "Content-Type": entry.mime,
          "Cache-Control": "public, max-age=31536000, immutable",
        },
      });
    },
  };
}
