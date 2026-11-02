#!/usr/bin/env bash
# Liga o MCP nativo do Meilisearch (v1.54+) e cria uma chave só de leitura para ele.
# Uso: MEILI_URL=http://localhost:7700 MEILI_MASTER_KEY=... scripts/meili-mcp-setup.sh
# Depois exporte MEILI_MCP_URL e MEILI_MCP_KEY para o .mcp.json do projeto.
set -euo pipefail
: "${MEILI_URL:=http://localhost:7700}"
: "${MEILI_MASTER_KEY:?defina MEILI_MASTER_KEY}"
auth=(-H "Authorization: Bearer ${MEILI_MASTER_KEY}" -H 'Content-Type: application/json')

echo "Ligando a feature experimental mcpRoute..."
curl -fsS -X PATCH "${MEILI_URL}/experimental-features" "${auth[@]}" -d '{"mcpRoute": true}'
echo

echo "Criando chave só leitura para o MCP..."
curl -fsS -X POST "${MEILI_URL}/keys" "${auth[@]}" -d '{
  "name": "claude-mcp-readonly",
  "description": "Claude Code MCP: search, documents.get, indexes.get",
  "actions": ["search", "documents.get", "indexes.get"],
  "indexes": ["*"],
  "expiresAt": null
}' | sed -n 's/.*"key":"\([^"]*\)".*/MEILI_MCP_KEY=\1/p'
echo
echo "MEILI_MCP_URL=${MEILI_URL}"
