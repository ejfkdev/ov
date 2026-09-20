#!/bin/zsh
# ov 一键发布: 打带描述的 tag → 推送 → 等 Release CI 出产物 → 触发 brew/scoop 更新。
#
# 用法:
#   scripts/release.sh v0.1.3                          # 默认 tag 描述
#   scripts/release.sh v0.1.3 -m "本轮变更摘要"          # 内联描述(即 tag 描述)
#   scripts/release.sh v0.1.3 -m notes.md              # 从文件读取描述
#
# 前置条件: 工作树干净、本机 gh 已登录(负责推 tag 与触发 tap/bucket 的更新工作流,
# 因此不需要任何 CI secret —— 与 ddc 的发布方式一致)。
#
# 幂等: tag 已存在则跳过打 tag, 仍会等 CI 并触发 tap/bucket 更新, 可安全重跑。
set -euo pipefail

REPO=ejfkdev/ov
TAP_REPO=ejfkdev/homebrew-tap
BUCKET_REPO=ejfkdev/scoop-bucket

ver=${1:-}
msg=""
if [[ ${2:-} == "-m" ]]; then
  msg=${3:-}
  [[ -z $msg ]] && { echo "-m 需要跟描述文本或描述文件路径" >&2; exit 2; }
fi
if [[ -z $ver ]]; then
  echo "usage: scripts/release.sh v<version> [-m <描述文本|描述文件>]" >&2
  exit 2
fi
ver=${ver#v}

echo "==> release ov $ver"
cd "$(dirname "$0")/.."

# ---- 1. 前置检查 -------------------------------------------------------------
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "worktree is dirty — commit first" >&2
  exit 1
fi
echo "==> build + vet"
go build ./...
go vet ./...

# ---- 2. tag(带描述) ----------------------------------------------------------
if git rev-parse -q --verify "v$ver" >/dev/null; then
  echo "==> tag v$ver 已存在, 跳过打 tag"
else
  echo "==> tag v$ver"
  if [[ -n $msg ]]; then
    if [[ -f $msg ]]; then
      git tag -a "v$ver" -F "$msg"
    else
      git tag -a "v$ver" -m "$msg"
    fi
  else
    printf 'v%s\n\nReleased via scripts/release.sh.\n' "$ver" | git tag -a "v$ver" -F -
  fi
  git tag -n1 "v$ver"
  git push -q origin "v$ver"
  echo "==> tag 已推送"
fi

# ---- 3. 等 Release CI --------------------------------------------------------
echo "==> waiting for the release workflow"
sleep 15
run=""
typeset -i i=0
while [[ -z $run ]]; do
  run=$(gh run list --repo "$REPO" --workflow release.yml --limit 1 \
    --json databaseId,status --jq '.[0].databaseId' 2>/dev/null)
  (( i += 1 )); (( i > 12 )) && { echo "no release run appeared" >&2; exit 1; }
  sleep 10
done
gh run watch "$run" --repo "$REPO" --exit-status >/dev/null
echo "==> release CI green: https://github.com/$REPO/actions/runs/$run"

# ---- 4. 触发 brew tap + scoop bucket ----------------------------------------
echo "==> dispatching homebrew-tap + scoop-bucket updates"
gh workflow run auto-update.yml --repo "$TAP_REPO"
gh workflow run auto-update.yml --repo "$BUCKET_REPO"
echo "    $TAP_REPO/actions (Auto-update formulae)"
echo "    $BUCKET_REPO/actions (Auto-update manifests)"
echo "==> done: brew install ejfkdev/tap/ov / scoop install ov"