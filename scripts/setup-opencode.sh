#!/usr/bin/env bash
set -euo pipefail

# Install OpenCode and point it at a viiwork node.
#
# A node that serves /api.json (viiwork 2.3 and later) is an OpenCode model
# catalogue in its own right: the client discovers every model on the mesh and
# keeps up as the fleet changes, so the config below lists none. See
# docs/autodiscovery.md.
#
# Against an older node there is nothing to discover, so the models are read
# from /v1/models once and written out — the arrangement this script used
# before, and one that goes stale the moment the fleet does.

read -rp "Viiwork host (IP or hostname, default localhost): " input
host="${input:-localhost}"

[[ "$host" != http://* && "$host" != https://* ]] && host="http://$host"
[[ ! "$host" =~ :[0-9]+$ ]] && host="${host}:8086"

if ! curl -sf "${host}/health" >/dev/null 2>&1 && ! curl -sf "${host}/v1/models" >/dev/null 2>&1; then
    echo "Host not reachable at ${host}" >&2
    exit 1
fi

# Install OpenCode
if ! command -v opencode &>/dev/null; then
    echo "Installing OpenCode..."
    curl -fsSL https://opencode.ai/install | bash
else
    echo "OpenCode already installed: $(command -v opencode)"
fi

# The node's own provider is the one advertising this host as its endpoint.
# With an upstream chained in, the document also carries every hosted provider
# models.dev knows, so "the first one" is somebody else's.
provider=""
count=0
# `|| true`: read returns non-zero at EOF, which is exactly the "no catalogue
# here, fall through" case, and set -e would take it for a failure.
read -r provider count < <(curl -sf "${host}/api.json" 2>/dev/null |
    HOST="$host" python3 -c '
import json, os, sys
from urllib.parse import urlparse

want = urlparse(os.environ["HOST"]).netloc
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for pid, p in doc.items():
    if urlparse(p.get("api") or "").netloc == want:
        print(pid, len(p.get("models") or {}))
        break
' 2>/dev/null || true) || true

if [ -n "$provider" ]; then
    cat > opencode.json <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "provider": { "${provider}": {} }
}
EOF

    echo ""
    echo "Wrote opencode.json — ${count} model(s), discovered from ${host}"
    echo ""
    echo "The model list is NOT in that file. OpenCode reads it from the node,"
    echo "which needs one environment variable:"
    echo ""
    echo "  export OPENCODE_MODELS_URL=${host}"
    echo ""
    echo "Put that in your shell profile, then run 'opencode'."
    exit 0
fi

# Fallback: an older node. Enumerate once and write the list out.
echo "${host} serves no /api.json — falling back to a static model list."
echo "Upgrade the node to discover models instead (docs/autodiscovery.md)."

MODELS=()
while IFS= read -r id; do
    [ -n "$id" ] && MODELS+=("$id")
done < <(curl -sf "${host}/v1/models" | python3 -c '
import json, sys
for m in json.load(sys.stdin).get("data", []):
    print(m["id"])
')

if [ ${#MODELS[@]} -eq 0 ]; then
    echo "No models found at ${host}/v1/models. Aborting." >&2
    exit 1
fi

echo "Detected ${#MODELS[@]} model(s):"
for m in "${MODELS[@]}"; do echo "  - ${m}"; done

# Context per slot, so the generated limits match what the backends will accept.
declare -A CTX=()
while IFS=$'\t' read -r name ctx; do
    [ -n "$name" ] && CTX["$name"]="$ctx"
done < <(curl -sf "${host}/v1/fleet/capacity" 2>/dev/null |
    python3 -c '
import json,sys
try:
    for m in json.load(sys.stdin).get("models", []):
        print(m["name"], m.get("ctx", 0), sep="\t")
except Exception:
    pass' 2>/dev/null || true)

models_json=""
for m in "${MODELS[@]}"; do
    ctx="${CTX[$m]:-8192}"
    [ "$ctx" -le 0 ] 2>/dev/null && ctx=8192
    out=$(( ctx / 4 ))
    [ "$out" -gt 32768 ] && out=32768
    [ "$out" -ge 1024 ] && out=$(( out - out % 1024 ))
    [ "$out" -lt 1 ] && out=1
    [ -n "$models_json" ] && models_json="${models_json},"
    models_json="${models_json}
        \"${m}\": {
          \"name\": \"${m}\",
          \"tool_call\": true,
          \"limit\": { \"context\": ${ctx}, \"output\": ${out} }
        }"
done

default_model="${MODELS[0]}"
if [ ${#MODELS[@]} -gt 1 ]; then
    echo ""
    echo "Which model should be the default?"
    for i in "${!MODELS[@]}"; do echo "  $((i+1))) ${MODELS[$i]}"; done
    read -rp "Choice (default 1): " choice
    choice="${choice:-1}"
    default_model="${MODELS[$((choice-1))]}"
fi

cat > opencode.json <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "provider": {
    "viiwork": {
      "name": "viiwork",
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "${host}/v1"
      },
      "models": {${models_json}
      }
    }
  },
  "model": "viiwork/${default_model}"
}
EOF

echo ""
echo "Wrote opencode.json"
echo "  Host:    ${host}"
echo "  Models:  ${#MODELS[@]} (static — re-run this script when the fleet changes)"
echo "  Default: ${default_model}"
echo ""
echo "Run 'opencode' to start."
