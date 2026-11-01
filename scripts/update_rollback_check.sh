#!/bin/bash
# 更新回滚故障注入检查：编译真实的 UpdateManager.swift（不是复制一份逻辑），
# 在临时目录里注入「新包只复制了一半」「旧包不见了」「恢复的包身份不符」
# 「回滚候选被按修改时间误删」四类故障，验证安装位置永远不会变空。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

xcrun swiftc -parse-as-library -o "$WORK/update-rollback-probe" \
  "$ROOT/app/Sources/UpdateManager.swift" \
  "$ROOT/app/Sources/ProcessRunner.swift" \
  "$ROOT/app/Sources/Models.swift" \
  "$ROOT/app/Tests/UpdateRollbackProbe.swift"

"$WORK/update-rollback-probe"
