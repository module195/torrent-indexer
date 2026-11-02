#!/usr/bin/env bash
# PostToolUse: formata com gofmt o arquivo .go que o Claude acabou de editar.
# Silencioso quando não há jq/gofmt ou o arquivo não é Go.
command -v jq >/dev/null 2>&1 || exit 0
command -v gofmt >/dev/null 2>&1 || exit 0
file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
  *.go) [ -f "$file" ] && gofmt -w "$file" ;;
esac
exit 0
