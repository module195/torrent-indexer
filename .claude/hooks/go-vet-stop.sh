#!/usr/bin/env bash
# Stop: roda `go vet ./...` quando há .go alterado (staged, unstaged ou novo).
# Se falhar, sai com 2 e devolve a saída ao Claude, que corrige antes de parar.
# Não roda de novo se o Claude já está continuando por causa deste hook.
command -v jq >/dev/null 2>&1 || exit 0
command -v go >/dev/null 2>&1 || exit 0
[ "$(jq -r '.stop_hook_active // false')" = "true" ] && exit 0
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
changed=$( { git diff --name-only HEAD -- '*.go'; git ls-files --others --exclude-standard -- '*.go'; } 2>/dev/null )
[ -n "$changed" ] || exit 0
if ! out=$(go vet ./... 2>&1); then
  printf 'go vet falhou (o CI também falharia). Corrija antes de terminar:\n%s\n' "$out" | tail -40 >&2
  exit 2
fi
exit 0
