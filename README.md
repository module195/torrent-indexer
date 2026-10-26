# torrent-indexer

[![](https://dcbadge.limes.pink/api/server/7wqNywmpQW)](https://discord.gg/7wqNywmpQW)
[![Sponsor](https://img.shields.io/badge/Sponsor-❤-ff69b4?style=for-the-badge&logo=github)](https://github.com/sponsors/felipemarinho97)

This is a simple torrent indexer that can be used to index torrents from HTML pages. It is written in Golang and uses Redis as a cache.

## Test it

Visit [https://torrent-indexer.darklyn.org/](https://torrent-indexer.darklyn.org/) to test it.

## Supported sites

- [comando-torrents](https://comando.la/)
- [bludv](https://bludvfilmes.tv/)
- [torrent-dos-filmes](https://torrentdosfilmes.se/)
- [starck-filmes](https://starckfilmes-v27.com/)
- [rede-torrent](https://redetorrent.com/)
- [vaca-torrent](https://vacatorrentmov.com/)

## Deploy

If you have Docker + docker-compose installed, you can deploy it using the following command:

```bash
git clone https://github.com/module195/torrent-indexer.git
cd torrent-indexer
MEILI_MASTER_KEY=uma-chave-segura docker compose up -d --build
```

The reference `docker-compose.yml` builds this fork and starts Redis, FlareSolverr and Meilisearch, with `RESPONSE_CACHE_TTL=5m` and `REQUEST_COALESCE=true` enabled (the settings guindex benefits from). Remove the services you don't need.

The server will be available at [http://localhost:8080/](http://localhost:8080/).

## Configuration

You can configure the server using the following environment variables:
  
- `PORT`: (optional) The port that the server will listen to. Default: `7006`
- `METRICS_PORT`: (optional) The port that the metrics server will listen to. Default: `8081`
- `LOG_LEVEL`: (optional) The log level. Default: `1` (info). 
  - Possible values: `0` (debug), `1` (info), `2` (warn), `3` (error)
- `LOG_FORMAT`: (optional) The log format. Can be "json" or default to console logger.
- `FLARESOLVERR_ADDRESS`: (optional) The address of the FlareSolverr instance. Default: `N/A`
- `FLARESOLVERR_TIMEOUT_SECONDS`: (optional) Timeout for flaresolverr requests. Default: `30`
- `FLARESOLVERR_POOL_SIZE`: (optional) Number of FlareSolverr sessions in the pool. Default: `5`
- `FLARESOLVERR_HEALTHCHECK_SECONDS`: (optional) Interval to verify pooled FlareSolverr sessions and recreate the ones lost after a FlareSolverr restart. Default: `N/A` (disabled)
- `FLARESOLVERR_SESSIONS`: (optional) `true`/`false` forces whether the solver supports `sessions.*`. By default it is detected from `GET /`: FlareSolverr uses a pool of sessions; Byparr (which only speaks `request.get`) gets `FLARESOLVERR_POOL_SIZE` concurrency slots and is never sent `sessions.*`. Default: `N/A` (auto)
- `REQUEST_COALESCE`: (optional) `true` makes concurrent requests for the same page URL share a single fetch. Default: `false`
- `RESPONSE_CACHE_TTL`: (optional) Caches indexer JSON responses (keyed by indexer + full query; a response with 0 results is kept for at most 2 minutes, and only when the site listed torrents that were all filtered out) and coalesces identical concurrent requests; the `X-Cache` header becomes `HIT|MISS|SHARED` (it is `BYPASS` while disabled). Duration format, e.g. `5m`. Default: `N/A` (disabled)
- `RESPONSE_CACHE_TTL_<NAME>`: (optional) Per-indexer override of `RESPONSE_CACHE_TTL`, where `NAME` is the indexer key in uppercase with underscores (e.g. `RESPONSE_CACHE_TTL_BETOR=30m`, `RESPONSE_CACHE_TTL_TORRENT_DOS_FILMES=2m`). `0` turns the response cache off for that indexer. Default: `N/A` (uses `RESPONSE_CACHE_TTL`)
- `TRACKER_SCRAPE_MAX_ADDITIONAL`: (optional) Max number of UDP trackers from the extra tracker list scraped per torrent (the magnet's own trackers are always used). Default: `N/A` (all)
- `TRACKER_SCRAPE_CONCURRENCY`: (optional) Global limit of concurrent UDP tracker scrapes. Default: `N/A` (unlimited)
- `REDIS_NAMESPACED_KEYS`: (optional) `true` prefixes Redis keys by type (`doc:`, `doc:short:`, `peers:`, `meta:`). Old keys expire on their own. Default: `false`
- `REQUEST_TIMEOUT_MILLISECONDS`: (optional) Timeout for external scraping requests. Default: `5000`
- `MEILISEARCH_ADDRESS`: (optional) The address of the MeiliSearch instance. Default: `N/A`
- `MEILISEARCH_KEY`: (optional) The API key of the MeiliSearch instance. Default: `N/A`
- `BETOR_DUMP`: (optional) `true` or `false` to turn the betor dump sync on or off without touching `BETOR_DUMP_URL`. `false` keeps it off even when `BETOR_DUMP_URL` is set; `true` turns it on and uses `https://catalogo-data.betor.top/items.json` when `BETOR_DUMP_URL` is empty. Needs `MEILISEARCH_ADDRESS`. Default: `N/A` (on when `BETOR_DUMP_URL` is set)
- `BETOR_DUMP_URL`: (optional) URL of the betor catalog dump (e.g. `https://catalogo-data.betor.top/items.json`). When set, together with `MEILISEARCH_ADDRESS`, the indexer loads the whole catalog into Meilisearch at start and then on every interval. Documents are upserted (PUT), so fields a scraper stored for the same hash are kept; base32 hashes become lowercase hex; a torrent whose name does not match the other names of its IMDb ID keeps its document but loses the IMDb link. A Redis lock keeps several replicas from running it at once. Default: `N/A` (disabled)
- `BETOR_DUMP_INTERVAL`: (optional) How often to reload `BETOR_DUMP_URL`, in duration format (minimum `1m`). Default: `8h`
- `REDIS_HOST`: (optional) The address of the Redis instance. Default: `localhost`
- `REDIS_PASSWORD`: (optional) The password of the Redis instance. Default: `N/A`
- `SHORT_LIVED_CACHE_EXPIRATION` (optional) The expiration time of the short-lived cache in duration format. Default: `30m`
    - This cache is used to cache homepage or search results.
    - Example: `30m`, `1h`, `1h30m`, `1h30m30s`
- `LONG_LIVED_CACHE_EXPIRATION` (optional) The expiration time of the long-lived cache in duration format. Default: `7d`
    - This cache is used to store the torrent webpages (posts). You can set it to a higher value because the torrent pages are not updated frequently.

### Extra Configuration
- `FALLBACK_TITLE_ENABLED`: (optional) Enable the fallback title post-processor that sets the title to "[UNSAFE] {original_title}" (Page title) if the title is empty. Default: `false`
    - This is useful for sites that do not have a title for some torrents, but can lead to misleading titles.
- `MAGNET_METADATA_API_ENABLED`: (optional) Enable the magnet metadata API. (deploy instrucitons [here](https://github.com/felipemarinho97/magnet-metadata-api)) Default: `false`
- `MAGNET_METADATA_API_ADDRESS`: (optional) The address of your magnet metadata API. Default: `N/A`
- `MAGNET_METADATA_API_TIMEOUT_SECONDS`: (optional) The timeout for the magnet metadata API requests in seconds. Default: `10`
- `INDEXER_<NAME>_URL`: (optional) Set a custom URL for the indexer. Where the "NAME" will be always uppercase indexer key with underscores. ex: `INDEXER_DODO_FILMES_URL=https://my-proxied-dodo-url.org`

## API for aggregating clients (e.g. guindex)

These additions are optional: existing routes and their JSON are unchanged.

- **`X-Cache` header** on every `/indexers/<name>` route: `HIT`, `MISS` or `SHARED` when served through `RESPONSE_CACHE_TTL` (or by `torrentfilmeshds`' own cache), `BYPASS` otherwise.
- **Cache bypass**: add `nocache=1` to an `/indexers/<name>` (or `/indexers/multi`) request, or send `Cache-Control: no-cache`, to skip the cached response and run the indexer again; the fresh result replaces the cached one. `nocache` is not part of the cache key. It skips only the response cache, not the page cache of `SHORT_LIVED_CACHE_EXPIRATION`, and `torrentfilmeshds` keeps its own cache.
- **`GET /sources?v=2`**: the same payload as `/sources` (including `indexer_names`) plus:
  - `api_version`: `2`
  - `indexers`: one entry per active indexer with `name`, `supports_text`, `supports_imdb`, `healthy` (false after 3 consecutive failed runs: HTTP 5xx/403/429; true again 5 minutes after the last failure so clients retry it, doubling on every failed retry up to 1 hour; always true during the first 2 minutes after start), `retry_after_s` (only while unhealthy: seconds until it is offered again), `consecutive_failures`, `last_failure_at` (RFC 3339, absent until a failure), `p50_ms` (median of the last 50 successful runs; absent until one) and `samples`. Stats live in memory and reset on restart; cached replays (`X-Cache: HIT`) are not counted.
  - `search`: `{"meilisearch": true|false}` (configured and answering; probed at most once a minute)
  - `features`: `{"multi": true, "x_cache": true, "response_cache": true|false}`

  An older server ignores `v=2` and answers the plain payload, so clients can check `api_version` and fall back.
- **`GET /indexers/multi?q=a&q=b&indexers=x,y&deadline_ms=8000`**: runs every indexer x query pair on the server (same handlers, cache and stats as `/indexers/<name>`) and answers when all finish or at the deadline, whichever comes first.
  - `q`: repeatable, up to 10 distinct values (trimmed, case-insensitive dedup). Required.
  - `indexers`: comma-separated names; default: all of `indexer_names`.
  - `deadline_ms`: default `8000`, clamped to `500..60000`. Calls still running at the deadline keep going in the background (up to 2 min) so their result lands in the response cache for the next request. At most 16 calls run at once, taken in order: every indexer for the first `q`, then the second, and so on, so the first `q` is never the one left waiting.
  - Any other parameter (`limit`, `sortBy`, `sortDirection`, `audio`, `year`, `imdb`, ...) is passed to each call; `limit` and the sort are applied again to the merged list.
  - Pairs an indexer can't answer are skipped instead of called (IMDb ids only go to `betor` and `bludv_ru`, text queries to all but `betor`).
  - Response: `results` (deduplicated by info hash: highest seeders/leechers and similarity, union of audio/trackers, plus `found_in` with every indexer that had it), `count`, `partial` (true if any call timed out) and `calls`: one entry per pair with `indexer`, `q`, `state` (`ok|error|skipped|timeout`), `status`, `count`, `duration_ms`, `cache` (the call's `X-Cache`) and `reason` (`imdb_not_supported`, `text_not_supported`, `unknown_indexer`, `not_started` for a call still queued at the deadline (its `duration_ms` is 0), or the indexer's error).
  - `/search` is unchanged and still only queries Meilisearch.

## Integrating with Jackett

You can integrate this indexer with Jackett by adding a new Torznab custom indexer. Here is an example of how to do it for the `bludv` indexer:

```yaml
---
id: bludv_indexer
name: BluDV Indexer
description: "BluDV - Custom indexer on from torrent-indexer"
language: pt-BR
type: public
encoding: UTF-8
links:
  - http://localhost:8080/

caps:
  categorymappings:
    - { id: Movie, cat: Movies, desc: "Movies" }
    - { id: TV, cat: TV, desc: "TV" }

  modes:
    search: [q]
    tv-search: [q, season, ep]
    movie-search: [q]
  allowrawsearch: true

settings: []

search:
  paths:
    - path: "indexers/bludv?filter_results=true&q={{ .Keywords }}"
      response:
        type: json

  keywordsfilters:
    - name: tolower

  rows:
    selector: $.results
    count:
      selector: $.count

  fields:
    _id:
      selector: title
    download:
      selector: magnet_link
    title:
      selector: title
    description:
      selector: original_title
    details:
      selector: details
    infohash:
      selector: info_hash
    date:
      selector: date
    size:
      selector: size
    seeders:
      selector: seed_count
    leechers:
      selector: leech_count
    imdb:
      selector: imdb
    category_is_tv_show:
      selector: title
      filters:
        - name: regexp
          args: "\\b(S\\d+(?:E\\d+)?)\\b"
    category:
      text: "{{ if .Result.category_is_tv_show }}TV{{ else }}Movie{{ end }}"
# json engine n/a
```

## Integrating with Prowlarr

You can integrate this indexer with Prowlarr by adding a custom definition. See [Adding a custom YML definition](https://wiki.servarr.com/prowlarr/indexers#adding-a-custom-yml-definition).

```yaml
---
id: torrent-indexer
name: Torrent Indexer
description: "Indexing Brazilian Torrent websites into structured data. github.com/module195/torrent-indexer"
language: pt-BR
type: public
encoding: UTF-8
links:
  - http://localhost:8080/

caps:
  categories:
    Movies: Movies
    TV: TV

  modes:
    search: [q]
    tv-search: [q, season]
    movie-search: [q]

settings:
  - name: indexer
    type: select
    label: Indexer
    default: bludv
    options:
      search: Torrent-Indexer Cache
      bludv: BLUDV
      comando_torrents: Comando Torrents
      torrent-dos-filmes: Torrent dos Filmes
      starck-filmes: Starck Filmes
      rede_torrent: Rede Torrent
      vaca_torrent: Vaca Torrent

search:
  paths:
    - path: "{{ if eq .Config.indexer \"search\" }}/search{{ else }}/indexers/{{ .Config.indexer }}{{ end }}"
      response:
        type: json
  inputs:
    filter_results: "true"
    q: "{{ .Keywords }}"
  keywordsfilters:
    - name: tolower
    - name: re_replace
      args: ["(?i)(S0)(\\d{1,2})$", "temporada $2"]
    - name: re_replace
      args: ["(?i)(S)(\\d{1,3})$", "temporada $2"]

  rows:
    selector: $.results
    count:
      selector: $.count

  fields:
    download:
      selector: magnet_link
    title:
      selector: title
    description:
      selector: original_title
    details:
      selector: details
    infohash:
      selector: info_hash
    date:
      selector: date
    size:
      selector: size
    seeders:
      selector: seed_count
    leechers:
      selector: leech_count
    imdb:
      selector: imdb
    category_is_tv_show:
      selector: title
      filters:
        - name: regexp
          args: "\\b(S\\d+(?:E\\d+)?)\\b"
    category:
      text: "{{ if .Result.category_is_tv_show }}TV{{ else }}Movies{{ end }}"
```


## Mirrors

- N/A

### Community Mirrors

If you're running a public instance of torrent-indexer and would like to share it with the community, please open a **Merge Request** adding your instance to this list!

**Requirements for community mirrors:**
- Must be publicly accessible
- Should have reasonable uptime
- Include geographic location information
- Provide contact information for maintenance issues

# Warning

The instance running at [https://torrent-indexer.darklyn.org/](https://torrent-indexer.darklyn.org/) is my personal instance and it is not guaranteed to be up all the time. Also, for better availability, I recommend deploying your own instance because the Cloudflare protection may block requests from indexed sites if too many requests are made in a short period of time from the same IP.

If I notice that the instance is being used a lot, I may block requests from Jackett/Prowlarr to avoid overloading the server. I can also redirect your requests to a fake instance without prior notice. Consider yourself warned.

## If you cannot self-host

**If you really need to use my [instance](https://torrent-indexer.darklyn.org) continuously without the risk of being IP blocked, please subscribe to my [GitHub Sponsors](https://github.com/sponsors/felipemarinho97) (any tier) and let me know about your support ❤️ via [Discord](https://discord.gg/7wqNywmpQW).**
