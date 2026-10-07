#!/bin/bash
# leak-scan.sh — 提交前 / CI 里扫「不该进公开仓库的东西」
#
# 为什么需要它：这个仓库是公开的，而开发过程中很容易顺手把本机的运营细节
# 写进提交信息或文档里 —— 比如本地号池有多少账号、余额多少、收信域名是什么。
# 那类东西不是凭据，但会暴露运营规模，属于不该外泄的信息。
#
# 两层模式：
#   ① 通用（写在本文件里，公开安全）：key 的形状、金额、账号规模
#   ② 私有（读 .leak-patterns，**已 gitignore**）：收信域名、账号邮箱前缀等
#      —— 这些串本身就不该出现在公开文件里，所以不能硬编码在本脚本中。
#
# 用法：
#   tools/leak-scan.sh            # 扫工作区 + 全部提交信息
#   tools/leak-scan.sh --staged   # 只扫暂存区 + 最后一条提交信息（给 pre-commit 用）
set -uo pipefail

# ① 通用模式（这些是「形状」，不含任何本机具体值）
PATTERNS=(
  'sk-cho-[A-Za-z0-9_-]{8,}'      # 上游 API key
  'fsgw_[A-Za-z0-9_-]{12,}'       # 本网关客户端 key
  'fsg_[a-f0-9]{24,}'             # 租户 key
  '(wbfs|wbk)_[A-Za-z0-9_-]{8,}'   # 其它渠道的 key
  '\$[0-9]{1,3}(,[0-9]{3})+'      # 带千分位的金额
  '[0-9]{3,} ?个账号'              # 账号规模（写成数字的）
)

# ② 私有模式（本机具体值，从 gitignore 的文件里读）
PRIVATE_FILE="${LEAK_PATTERNS_FILE:-.leak-patterns}"
if [ -f "$PRIVATE_FILE" ]; then
  while IFS= read -r line; do
    case "$line" in ''|'#'*) continue ;; esac
    PATTERNS+=("$line")
  done < "$PRIVATE_FILE"
else
  echo "（提示：没有 $PRIVATE_FILE —— 只跑通用模式；本机具体值请写进那个文件）"
fi

RE=''
for p in "${PATTERNS[@]}"; do RE="${RE:+$RE|}$p"; done

fail=0
report() { # $1=来源描述  $2=命中内容
  [ -z "$2" ] && return
  echo "✘ $1 命中敏感模式："
  echo "$2" | head -10 | sed 's/^/    /'
  fail=1
}

if [ "${1:-}" = "--staged" ]; then
  report "暂存区" "$(git diff --cached -- . 2>/dev/null | grep -nE "$RE")"
  report "最后一条提交信息" "$(git log -1 --format=%B | grep -nE "$RE")"
else
  echo "扫描工作区文件…"
  report "工作区" "$(git grep -nE "$RE" -- . 2>/dev/null)"
  echo "扫描全部提交信息…"
  report "提交历史（信息）" "$(git log --all --format=%B | grep -nE "$RE")"
  # 也要扫**历史文件内容** —— 否则「某个提交里写过、后来删掉了」的串会被漏掉
  echo "扫描历史文件内容…"
  report "提交历史（文件内容）" "$(git log --all -p -- . 2>/dev/null | grep -nE "^\+.*($RE)" | sed 's/^/  /')"
fi

if [ "$fail" = "0" ]; then echo "✅ 没扫到敏感信息"; fi
exit "$fail"
