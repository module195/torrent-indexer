---
name: novo-indexer-scraper
description: Roteiro para investigar um site de torrent novo e adicioná-lo como indexer no torrent-indexer (Go), com amostras HTML, parser testado e registro nas rotas e tiers. Use quando pedirem para adicionar, trocar ou consertar o scraper de um site.
---

# Adicionar ou consertar um indexer

Siga as etapas em ordem. Não teste parser contra o site ao vivo: salve o HTML e teste contra o arquivo.

## 1. Investigar o site

1. Abra a home, uma busca (`?s=homem`), a página 2 e 3 a 5 páginas de título (um filme, uma série com temporada, uma coleção/pack). Use o Playwright MCP se o site tiver Cloudflare ou JS; senão `curl -A "Mozilla/5.0"`.
2. Anote: padrão da URL de busca e de paginação, seletor dos cards na listagem, onde ficam os magnets (botão por qualidade? por episódio?), título original, ano, IMDb (link ou só texto), áudio (Dublado/Dual/Legendado, "Áudio: Português"), tamanho (no botão, no texto, ou só `xl=` do magnet).
3. Confirme se precisa de FlareSolverr (resposta 403/503 com desafio). Se precisar, o `requester` já trata.

## 2. Salvar amostras

- `api/testdata/<indexer>/`: `home.html`, `busca-<termo>.html`, `pagina-2.html` e as páginas de título. Nomes descritivos em minúsculas com hífen.

## 3. Escrever o scraper (`api/<indexer>.go`)

Copie a estrutura de um indexer parecido (`rede_torrent.go` é o mais completo):
- `var <indexer> = IndexerMeta{Label, URL, SearchURL, PagePattern}`; o `Label` vai para métricas (alfanumérico com `_`).
- `func (i *Indexer) Handler<Nome>Indexer(w, r)`: métricas no `defer`, lê `q` e `page`, monta a URL de listagem, busca links, abre cada post em paralelo, chama os `GlobalPostProcessors` e responde `Response{Results, Count}`.
- Funções puras de extração recebendo `*goquery.Document` (`extract<Nome>Links`, `parse<Nome>Page`): é o que os testes cobrem.
- Respeite `INDEXER_<NOME>_URL` (override de URL) como os outros.
- Tamanho: prefira o do botão/magnet; títulos sem carimbo do site (o `CleanupTitleWebsites` ajuda, mas não conte só com ele).

## 4. Testes (`api/<indexer>_test.go`)

- Links da listagem: quantidade exata e só páginas de título (sem menu/categoria).
- Página de título: nº de magnets, info hash, título, ano, IMDb, áudio, tamanho, e `files` em packs.
- Rode: `go test -race ./api/ -run <Nome>` e depois `go test -race -shuffle=on ./...`.

## 5. Registrar

1. `main.go`: entrada em `indexerRoutes` com `respCache.Wrap("<nome>", stats.Track("<nome>", indexers.Handler<Nome>Indexer))`.
2. `api/capabilities.go`: entrada em `ActiveIndexers` com `SupportsText`/`SupportsIMDb` e `Tier` (primary/secondary/reserve + `ReserveFor`). Sem isso ele não aparece no `/sources` nem no `/indexers/multi`.
3. `api/index.go`: nome em `IndexerNames`, na mesma ordem de `ActiveIndexers`.
4. README: lista "Supported sites" e, se mudou tier, a seção do `/indexers/multi`.

## 6. Validar na stack

- `docker compose up -d --build` e `curl 'localhost:8080/indexers/<nome>?q=homem'`, depois `/indexers/multi?q=homem` e `/sources?v=2`.
- `make lint`.
- No PR, diga ao usuário se o guindex precisa de algo (normalmente não: ele lê `/sources?v=2`).
