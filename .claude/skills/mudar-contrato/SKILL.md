---
name: mudar-contrato
description: Roteiro para mudar a API que o guindex consome (JSON de /indexers/<nome>, /indexers/multi, /sources?v=2, /search, rotas, parâmetros ou X-Cache) sem quebrar o addon, com README, testes dos dois lados e os dois PRs na ordem certa.
disable-model-invocation: true
argument-hint: "<o que mudar, ex.: adicionar campo size_bytes em IndexedTorrent>"
---

# Mudar o contrato torrent-indexer → guindex

Mudança pedida: $ARGUMENTS

O torrent-indexer é a fonte da verdade e vai para produção primeiro. O guindex precisa continuar funcionando com a versão antiga **e** com a nova do indexer durante a transição.

## 1. Planeje a compatibilidade

- **Campo novo**: sempre opcional no Go (`omitempty`/`omitzero` quando zero não tem sentido). O guindex trata ausência.
- **Renomear ou trocar tipo**: não faça em um passo. Adicione o campo novo ao lado do antigo, migre o guindex para ler o novo com fallback para o antigo, e só remova o antigo num PR posterior (anote isso no PR).
- **Remover campo, rota ou parâmetro**: confirme no guindex (`grep` em `src/` e `tests/`) que ninguém usa. Se usa, é um rename (item acima).
- **Valor novo de enum em string** (`state`, `tier`, `X-Cache`): confira como o guindex trata valor desconhecido antes.

Escreva o plano em 3 a 5 linhas antes de editar.

## 2. torrent-indexer (este repo)

1. Mude a struct/rota (`schema/`, `api/multi.go`, `api/capabilities.go`, `api/search.go`, `main.go`).
2. Teste em Go cobrindo o JSON novo (forma serializada, não só o valor da struct). Siga os testes existentes em `api/*_test.go`.
3. Atualize o `README.md`, seção "API for aggregating clients": o que mudou, se é opcional, desde quando.
4. Rode: `go build ./... && go test -race -shuffle=on ./... && make lint && go mod tidy` (commite o resultado do tidy se mudar).
5. Rode o subagente `revisor-contrato` e resolva o que ele marcar como Quebra.
6. Commit, push e PR no `module195/torrent-indexer`. No corpo, diga explicitamente que é compatível com o guindex atual e qual PR do guindex acompanha.

## 3. guindex (`module195/guindex`, normalmente em `../guindex`)

1. Tipos em `src/services/torrent-indexer-shared.ts` (`TorrentResult`, `IndexerCapability`, `IndexerCatalog`, `MultiCall`): campo novo como opcional.
2. Leitura em `src/services/torrent-indexer-provider.ts`, com fallback quando o campo não vier (indexer antigo).
3. Servidor falso em `tests/indexer-integration.test.mjs` (e `tests/addon-indexer.test.mjs` se aplicável): um caso com o campo novo e um sem.
4. Se o campo chega ao stream do Stremio, siga também a skill `contrato-aiostreams` do guindex.
5. Rode: `npm run typecheck && npm run lint && npm test` (o `npm test` faz o build antes).
6. Commit, push e PR no `module195/guindex`, citando o PR do indexer e dizendo que funciona com e sem a mudança.

## 4. Ordem de merge

1. PR do torrent-indexer primeiro (compatível com o guindex atual).
2. PR do guindex depois.
3. Remoção de campo antigo, se houver, só num terceiro PR no indexer, depois que o guindex em produção já usa o novo.
