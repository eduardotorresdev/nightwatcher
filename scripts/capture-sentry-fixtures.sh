#!/usr/bin/env bash
# Captura Ocorrências reais do Sentry do acme e as grava como fixtures.
#
# Existe porque o PRD exige payloads gravados do real, não inventados, e porque as
# fixtures que estão em testdata/sentry/ hoje são SINTÉTICAS — ver o INDEX.md de lá.
# Este script é o caminho para trocá-las.
#
#   SENTRY_TOKEN=... scripts/capture-sentry-fixtures.sh <org> <projeto-slug> [n]
#
# O token precisa de escopo de leitura de issues (event:read / project:read).
#
# ATENÇÃO: a saída da API REST de issue é o MESMO serializer do webhook mais três
# URLs — ela NÃO é um payload de webhook. Este script grava o objeto issue já
# embrulhado no envelope da Integration Platform, que é o que o handler recebe.
# Um payload de webhook 100% fiel só se captura interceptando a entrega real; para
# isso, aponte a integration para um endpoint de captura e grave o corpo cru.
set -euo pipefail

ORG="${1:?uso: capture-sentry-fixtures.sh <org> <projeto-slug> [n]}"
PROJECT="${2:?uso: capture-sentry-fixtures.sh <org> <projeto-slug> [n]}"
LIMIT="${3:-10}"
: "${SENTRY_TOKEN:?exporte SENTRY_TOKEN}"

OUT="$(cd "$(dirname "$0")/.." && pwd)/testdata/sentry/real"
mkdir -p "$OUT"

api() { curl -sS -H "Authorization: Bearer $SENTRY_TOKEN" "https://sentry.io/api/0$1"; }

echo "==> lendo issues de $ORG/$PROJECT"
issues="$(api "/projects/$ORG/$PROJECT/issues/?limit=$LIMIT&query=")"

# installation.uuid só existe na entrega real; aqui fica um marcador visível.
INSTALL_UUID="${SENTRY_INSTALLATION_UUID:-CAPTURA-SEM-INSTALLATION-UUID}"

echo "$issues" | python3 -c '
import json, os, sys, re

issues = json.load(sys.stdin)
out = os.environ["OUT"]
uuid = os.environ["INSTALL_UUID"]

def anon(o):
    """Remove dado de usuário antes de versionar."""
    if isinstance(o, dict):
        return {k: ("usuario@exemplo.invalid" if k in ("contact_email", "email")
                    else "0.0.0.0" if k in ("ip_address",)
                    else anon(v))
                for k, v in o.items()}
    if isinstance(o, list):
        return [anon(v) for v in o]
    if isinstance(o, str):
        o = re.sub(r"[\w.+-]+@[\w-]+\.[\w.]+", "usuario@exemplo.invalid", o)
        o = re.sub(r"\b\d{1,3}(\.\d{1,3}){3}\b", "0.0.0.0", o)
    return o

for it in issues:
    it = anon(it)
    sub = it.get("substatus") or "new"
    action = "created" if sub == "new" else "unresolved"
    env = {"action": action,
           "installation": {"uuid": uuid},
           "data": {"issue": it},
           "actor": {"type": "application", "id": "sentry", "name": "Sentry"}}
    name = f"{it.get(\"issueCategory\",\"error\")}-{sub}-{it[\"id\"]}.json"
    with open(os.path.join(out, name), "w", encoding="utf-8") as f:
        json.dump(env, f, ensure_ascii=False, separators=(", ", ": "))
    print("  gravado", name)
' 2>&1

echo "==> tags de environment"
echo "$issues" | python3 -c '
import json,sys
for it in json.load(sys.stdin): print(it["id"])
' | while read -r id; do
  api "/issues/$id/tags/environment/" > "$OUT/tags-environment-$id.json" 2>/dev/null || true
done

echo
echo "==> gravado em $OUT"
echo "Revise ANTES de versionar: confirme que nenhum dado de usuário e nenhum token sobrou."
echo "Depois atualize testdata/sentry/INDEX.md."
