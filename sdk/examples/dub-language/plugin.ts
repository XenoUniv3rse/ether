import { plugin, type Manifest, type CandidateFacts } from '../../typescript/src/index.ts';
import manifest from './manifest.json' with { type: 'json' };

type Config = { value?: string[] };
const aliases: Record<string,string> = { english:'en', eng:'en', french:'fr', fre:'fr', fra:'fr', german:'de', ger:'de', deu:'de', spanish:'es', spa:'es', italian:'it', ita:'it', japanese:'ja', jpn:'ja', portuguese:'pt', por:'pt', korean:'ko', kor:'ko', chinese:'zh', chi:'zh', zho:'zh', hindi:'hi', hin:'hi' };
// normalize maps a language name, ISO code or regional variant
// ("spanish-latino", "pt-BR") to its two-letter base.
function normalize(s: string): string {
  let v = s.toLowerCase().trim();
  const dash = v.indexOf('-');
  if (dash > 0) v = v.slice(0, dash);
  return aliases[v] || v;
}
// available reads the audio languages Ether parsed from the release name
// and indexer tags. Ether's parser skips title words ("The French
// Dispatch") that a token scan would count, and re-reading the name here
// cost most of the step's time: ranking runs on the hot path, where a
// 500-release batch shares one 50 ms budget.
function available(c: CandidateFacts): Set<string> {
  const langs = new Set<string>();
  const list = c.languages || [];
  for (let i = 0; i < list.length; i++) langs.add(normalize(list[i]));
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
