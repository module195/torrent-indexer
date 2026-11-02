---
name: testar-busca-stack
description: Sobe a stack do torrent-indexer (Redis, FlareSolverr, Meilisearch) e roda a bateria fixa de buscas de filmes e séries, comparando indexers, /indexers/multi e /search. Use para validar uma mudança de scraper, de cache ou de busca antes do PR, ou para medir a stack.
---

# Bateria de buscas da stack

## 1. Subir

```bash
MEILI_MASTER_KEY=dev-key docker compose up -d --build
curl -fsS localhost:8080/sources?v=2 | head -c 400   # espera os indexers aparecerem
```

Se for testar uma instância remota, pule esta etapa e use `BASE=https://...` abaixo. Só faça GETs em produção.

## 2. Rodar a bateria

```bash
BASE=http://localhost:8080 bash scripts/testar-buscas.sh
```

Gera `buscas-<data>/` com um `.json` e `.headers` por chamada e um `RESUMO.txt`. Casos fixos: Oppenheimer, Duna parte dois, Ainda estou aqui, The Last of Us, Round 6, The Boys T4, cada um por texto e por IMDb em cada indexer, no `multi` e no `/search`.

## 3. O que conferir

- **Cobertura:** `count` por indexer e no `multi`; um indexer primário com 0 em casos que antes achava é regressão.
- **Metadados:** título sem carimbo do site, `year`, `imdb`, `audio` (pt-BR deve vir como `brazilian`), `size` preenchido, `files` em packs.
- **Cache:** segunda rodada deve vir com `X-Cache: HIT` (com `RESPONSE_CACHE_TTL` ligado).
- **Tempo:** `duration_ms` em `calls` do `multi`; nada primário estourando o `deadline_ms`.
- **Meilisearch:** `/search?q=<imdb>` não pode voltar 0 se o dump do betor estiver carregado (bug I1). Use o MCP `meilisearch` para inspecionar documentos.
- **Redis:** use o MCP `redis` para ver chaves e TTLs se o cache não estiver batendo.

## 4. Reportar

Tabela curta por caso × indexer (count, tempo, cache) e lista do que mudou contra a rodada anterior. Não cole os JSON inteiros.
