import { plugin, type Manifest, type CandidateFacts } from '../../typescript/src/index.ts';
import manifest from './manifest.json' with { type: 'json' };

type Config = { value?: string[] };
const aliases: Record<string,string> = { english:'en', eng:'en', french:'fr', fre:'fr', fra:'fr', german:'de', ger:'de', deu:'de', spanish:'es', spa:'es', italian:'it', ita:'it', japanese:'ja', jpn:'ja', portuguese:'pt', por:'pt' };
const codes = new Set(['en','fr','de','es','it','ja','pt','ko','zh','hi']);
const separators = /[^a-z0-9]+/;
function normalize(s: string): string { const v = s.toLowerCase().trim(); return aliases[v] || v; }
// available is computed once per candidate: ranking runs on the hot path,
// where a 500-release batch shares one 50 ms budget.
function available(c: CandidateFacts): Set<string> {
  const langs = new Set<string>();
  for (const l of c.languages || []) langs.add(normalize(l));
  for (const token of (c.name || '').toLowerCase().split(separators)) {
    const alias = aliases[token];
    if (alias) langs.add(alias); else if (codes.has(token) || token === 'multi') langs.add(token);
  }
  return langs;
}
function speaks(langs: Set<string>, lang: string): boolean {
  const target = normalize(lang);
  return !!target && (langs.has(target) || langs.has('multi'));
}
export const dubPlugin = plugin<Config>({ manifest: manifest as Manifest, routes: {
  rankKey(c, cfg) {
    const wanted = Array.isArray(cfg?.value) ? cfg.value : [];
    if (wanted.length === 0) return 0;
    const langs = available(c);
    for (let i = 0; i < wanted.length; i++) if (speaks(langs, wanted[i])) return wanted.length - i;
    return 0;
  },
  condMatch(c, op, value) {
    if (op === 'equals') return typeof value === 'string' && speaks(available(c), value);
    if (op === 'in') { const values = Array.isArray(value) ? value : typeof value === 'string' ? value.split(',') : []; const langs = available(c); return values.some(v => typeof v === 'string' && speaks(langs, v)); }
    return false;
  }
}});
export const handle = dubPlugin.handle;
export const rank_key = dubPlugin.rank_key;
export const cond_match = dubPlugin.cond_match;
