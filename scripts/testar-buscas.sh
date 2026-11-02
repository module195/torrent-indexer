#!/usr/bin/env bash
# Testa buscas de filmes e séries no torrent-indexer (somente leitura: só faz GET).
# Uso:  bash testar-buscas.sh            (padrão: http://localhost:8080, a stack do docker-compose)
#       BASE=http://localhost:7006 bash testar-buscas.sh
set -u
BASE="${BASE:-http://localhost:8080}"
OUT="buscas-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"
UA="torrent-indexer-teste/1.0"

INDEXERS="torrent-dos-filmes comando_torrents rede_torrent torrentfilmeshds bludv_ru baixarapido"

# nome|consulta texto|imdb
CASOS=(
  "filme_oppenheimer|oppenheimer|tt15398776"
  "filme_duna2|duna parte dois|tt15239678"
  "filme_ainda_estou_aqui|ainda estou aqui|tt14961016"
  "serie_the_last_of_us|the last of us|tt3581920"
  "serie_round6|round 6|tt10919420"
  "serie_the_boys_s04|the boys 4 temporada|tt1190634"
)

get() { # arquivo url
  local f="$1" u="$2"
  curl -sS -A "$UA" -m 70 -D "$OUT/$f.headers" -o "$OUT/$f.json" \
       -w "%{http_code} %{time_total}s %{size_download}B" "$u" 2>>"$OUT/erros.log"
  echo "  -> $f  $(cat "$OUT/$f.headers" 2>/dev/null | grep -i '^x-cache' | tr -d '\r')"
}
enc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1]))' "$1" 2>/dev/null || echo "${1// /+}"; }

echo "BASE=$BASE  saída=$OUT"
echo "== /sources";       get sources       "$BASE/sources"
echo "== /sources?v=2";   get sources_v2    "$BASE/sources?v=2"
echo "== /search/health"; get search_health "$BASE/search/health"
echo "== /search/stats";  get search_stats  "$BASE/search/stats"

for c in "${CASOS[@]}"; do
  IFS='|' read -r nome txt imdb <<<"$c"
  qt=$(enc "$txt")
  echo "== $nome  ('$txt' / $imdb)"
  for ix in $INDEXERS; do
    printf "%s" "$ix: "; get "${nome}__${ix}" "$BASE/indexers/$ix?q=$qt"
  done
  printf "betor(imdb): "; get "${nome}__betor_imdb" "$BASE/indexers/betor?q=$imdb"
  printf "betor(texto): "; get "${nome}__betor_texto" "$BASE/indexers/betor?q=$qt"
  printf "multi: "; get "${nome}__multi" "$BASE/indexers/multi?q=$qt&q=$imdb&deadline_ms=20000"
  printf "search(texto): "; get "${nome}__search_texto" "$BASE/search?q=$qt&limit=50"
  printf "search(imdb): "; get "${nome}__search_imdb" "$BASE/search?q=$imdb&limit=50"
done

# Resumo compacto (precisa de python3; se não houver, envie só o .tar.gz)
python3 - "$OUT" > "$OUT/RESUMO.txt" <<'PY' || echo "python3 indisponível: resumo não gerado"
import json, os, sys, collections
d = sys.argv[1]
F = ["title","original_title","year","imdb","audio","size","seed_count","leech_count","info_hash","files","date","trackers","similarity","indexer","magnet_link","details"]
for fn in sorted(os.listdir(d)):
    if not fn.endswith(".json"): continue
    p = os.path.join(d, fn)
    try: j = json.load(open(p))
    except Exception as e:
        print(f"\n### {fn}: JSON inválido ({e}) {open(p,errors='replace').read(200)!r}"); continue
    if not isinstance(j, dict) or "results" not in j:
        print(f"\n### {fn}\n{json.dumps(j, ensure_ascii=False)[:1500]}"); continue
    r = j.get("results") or []
    extra = {k: j[k] for k in ("count","indexed_count","partial") if k in j}
    print(f"\n### {fn}: {len(r)} resultados {extra}")
    if "calls" in j:
        for c in j["calls"]:
            print("   call", {k: c.get(k) for k in ("indexer","q","state","status","count","duration_ms","cache","reason")})
    if not r: continue
    fill = collections.Counter()
    for x in r:
        for k in F:
            v = x.get(k)
            if v not in (None, "", [], 0, "0001-01-01T00:00:00Z"): fill[k] += 1
    print("   preenchimento:", ", ".join(f"{k}={fill[k]*100//len(r)}%" for k in F))
    seeds = [x.get("seed_count",0) for x in r]
    print(f"   seeds: max={max(seeds)} zero={sum(1 for s in seeds if not s)}  imdbs={collections.Counter(x.get('imdb','') for x in r).most_common(4)}")
    print(f"   anos={collections.Counter(x.get('year','') for x in r).most_common(5)}  found_in={collections.Counter(i for x in r for i in (x.get('found_in') or [x.get('indexer','?')])).most_common()}")
    for x in r[:6]:
        print("   -", json.dumps({k: (x.get(k) if k!="files" else len(x.get("files") or [])) for k in ("title","original_title","year","imdb","audio","size","seed_count","leech_count","similarity","info_hash","files","indexer")}, ensure_ascii=False))
PY

tar czf "$OUT.tar.gz" "$OUT"
echo
echo "Pronto. Envie o arquivo $PWD/$OUT.tar.gz na conversa (ou cole $OUT/RESUMO.txt)."
