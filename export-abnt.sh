#!/usr/bin/env bash
set -euo pipefail
if [[ $# != 2 ]]; then printf '%s\n' 'Usage: export-abnt.sh NOTE.md OUTPUT.docx'; exit 2; fi
relay_script_root="$(cd "$(dirname "$0")" && pwd)"
command -v pandoc >/dev/null
relay_bibliography="${RELAY_BIBLIOGRAPHY:-${RELAY_SPACE:-.}/Bibliography/references.json}"
pandoc "$1" --citeproc --bibliography="$relay_bibliography" --csl="$relay_script_root/bibliography/abnt.csl" --metadata=lang:pt-BR -o "$2"
