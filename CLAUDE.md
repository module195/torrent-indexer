# torrent-indexer (fork module195)

Indexador em Go 1.25 que raspa sites brasileiros de torrent e expõe JSON. É a fonte de busca do **guindex** (addon Stremio, repo `module195/guindex`), que entrega streams via TorBox/Real-Debrid e é consumido pelo AIOStreams.

## Comandos

- Build: `go build ./...` · Rodar: `go run main.go` (porta 7006, métricas em 8081)
- Testes: `go test -race -shuffle=on ./...` (é o que o CI roda)
- Lint: `make lint` (golangci-lint v2, config em `.golangci.yml`)
- Stack completa: `MEILI_MASTER_KEY=... docker compose up -d --build` → http://localhost:8080 (indexer + Redis + FlareSolverr + Meilisearch v1.55)

## Mapa do código

- `main.go`: lê env, monta `indexerRoutes` (nome → handler com `respCache.Wrap` + `stats.Track`) e as rotas.
- `api/`: um arquivo por site (`bludv_ru.go`, `rede_torrent.go`, `starck_filmes.go`, `betor.go`, `torrentfilmeshds.go`, `baixarapido.go`…), mais `multi.go` (`/indexers/multi`), `capabilities.go` (`ActiveIndexers`, tiers), `response_cache.go`, `search.go`, `post_processors.go`, `betor_dump.go`.
- `api/testdata/<indexer>/`: HTML real usado nos testes de parser.
- `requester/`: HTTP + FlareSolverr/Byparr (pool de sessões). `scrape/`: seeders via UDP trackers. `search/meilisearch.go`. `schema/`: `IndexedTorrent`, regex de áudio. `monitoring/prometheus.go`: métricas.

## Contrato com o guindex (não quebrar)

- `GET /indexers/<nome>`, `GET /indexers/multi?q=..&indexers=..&deadline_ms=..`, `GET /sources?v=2`, `GET /search` (só Meilisearch).
- Header `X-Cache: HIT|MISS|SHARED|BYPASS`; `nocache=1` pula o cache de resposta.
- Tiers em `api/capabilities.go`: primary (bludv_ru, rede_torrent, starck-filmes, betor), secondary (torrentfilmeshds), reserve (torrent-dos-filmes, comando_torrents, baixarapido).
- Mudou JSON ou rota? Atualize o README ("API for aggregating clients") e avise que o guindex precisa acompanhar.

## Regras

- Novo indexer: use a skill `novo-indexer-scraper`. Todo indexer ativo precisa de rota em `main.go` (senão `Fatal` no start) e entrada em `ActiveIndexers`.
- Parser sempre testado contra HTML salvo em `api/testdata/`, nunca contra o site ao vivo.
- Não remova `respCache.Wrap`/`stats.Track` de rotas: o `/sources?v=2` e o cache dependem deles.
- Respostas externas devem ter limite de tamanho e timeout (auditoria de segurança 28/10).

## Ferramentas do projeto (Claude Code)

- Plugins (`.claude/settings.json`): gopls-lsp, security-guidance, pr-review-toolkit, commit-commands, claude-md-management, playwright, context7. Precisa de `gopls` no PATH: `go install golang.org/x/tools/gopls@latest`.
- MCPs (`.mcp.json`): `gopls` (MCP do gopls), `meilisearch` (MCP nativo, só leitura) e `redis`. Variáveis: `MEILI_MCP_URL`, `MEILI_MCP_KEY`, `REDIS_MCP_URL`. Para criar a chave do Meilisearch: `scripts/meili-mcp-setup.sh`. O Redis MCP precisa de `uv` (`uvx`); na primeira vez o download leva ~30 s e pode estourar o timeout de conexão, então rode antes `uvx --from redis-mcp-server@latest redis-mcp-server --help`. Ele expõe ferramentas de escrita (`set`, `delete`...): aponte para um usuário Redis só leitura (ACL) fora do ambiente local.
- Hook: `gofmt -w` em cada `.go` editado.
- Skills: `novo-indexer-scraper`, `testar-busca-stack`.

## Contexto extra

Relatórios de investigação (scraping por site, comparação de indexers, auditoria, mapa funcional) ficam na pasta compartilhada do projeto Claude, não neste repo.
