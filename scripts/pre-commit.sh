#!/bin/bash
BLOCKED=(
  "configs/config.yaml"
  "configs/config.dev.yaml"
  "configs/configs/"
  "crypto-admin"
)

STAGED=$(git diff --cached --name-only)
if [ -z "$STAGED" ]; then
  exit 0
fi

FOUND=""
for f in $STAGED; do
  for p in "${BLOCKED[@]}"; do
    case "$f" in
      $p) FOUND="$FOUND   $f\n" ;;
    esac
  done
done

if [ -n "$FOUND" ]; then
  printf "❌ 禁止提交敏感文件：\n$FOUND"
  echo ""
  echo "已在 .gitignore 中，请检查为何仍被加入暂存区。"
  exit 1
fi
