#!/usr/bin/env bash
# login.sh — WorkBuddy / CodeBuddy OAuth 登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh                     # 国内站（cn）
#   ./login.sh --site=intl         # 官方国际站（www.codebuddy.ai）
#   ./login.sh --site=workbuddy    # 旧桌面端国际版（www.workbuddy.ai）
#   ./login.sh --realm=global      # 兼容旧写法，等价 --site=workbuddy
#
# 站点表对齐官方 CLI 2.174.0 的 product.json 域表；--site 优先于 --realm。
#
# 流程:
#   1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   2. 你在浏览器打开 URL 完成登录（链接已带官方 version 参数）
#   3. 回到这里按 y → poll 拿 token+uid+nickname → 签到（仅 CN）→ 落盘 %APPDATA%\workbuddy2api\workbuddy-<uid>.json
#   4. 重启 workbuddy2api 容器加载新账号
set -euo pipefail

cd "$(dirname "$0")"

# 默认与服务端保持一致：Windows 用 %APPDATA%\workbuddy2api\auths，其他平台同理。
# Git Bash 里的 %APPDATA% 是 Windows 路径，cygpath 转成POSIX路径再拼。
if [[ -n "${APPDATA:-}" ]]; then
    if command -v cygpath >/dev/null 2>&1; then
        AUTH_DIR="$(cygpath -u "$APPDATA")/workbuddy2api"
    else
        AUTH_DIR="${APPDATA}/workbuddy2api"
    fi
elif [[ -n "${XDG_CONFIG_HOME:-}" ]]; then
    AUTH_DIR="${XDG_CONFIG_HOME}/workbuddy2api"
else
    AUTH_DIR="${HOME}/.config/workbuddy2api"
fi
CONTAINER="workbuddy2api"

# ─── 站点参数（官方域表：cn / intl / workbuddy）─────────────────
SITE=""
REALM=""
for arg in "$@"; do
    case "$arg" in
        --site=*)  SITE="${arg#--site=}" ;;
        --realm=*) REALM="${arg#--realm=}" ;;
        -h|--help)
            sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "未知参数: $arg（可用 --site=cn|intl|workbuddy、--realm=cn|global）" >&2
            exit 1
            ;;
    esac
done

# 站点 → 展示名/上游域名（与 internal/loginsite 的站点表一致）
site_label() {
    case "$1" in
        intl)      echo "官方国际站（www.codebuddy.ai）" ;;
        workbuddy) echo "旧桌面端国际版（www.workbuddy.ai）" ;;
        *)         echo "国内站（copilot.tencent.com）" ;;
    esac
}

mkdir -p "$AUTH_DIR"

# login 工具：不存在才编译（源码改动后手动 go build -o login ./cmd/login）
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

# 把 --site/--realm 归一化成实际站点（由 login 工具输出，避免脚本里重复维护域表）
if [[ -n "$SITE" ]]; then
    SITE_ARGS=(--site="$SITE")
elif [[ -n "$REALM" ]]; then
    SITE_ARGS=(--realm="$REALM")
else
    SITE_ARGS=()
fi
SITE=$("$LOGIN_BIN" "${SITE_ARGS[@]}" site)

echo "============================================================"
echo "  WorkBuddy OAuth 登录 · $(site_label "$SITE")"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" "${SITE_ARGS[@]}" url)

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" "${SITE_ARGS[@]}" poll) || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    echo "  - 上游返回许可类终态错误（席位满/许可证过期/试用到期）时，上面会直接给出 code 与 msg"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 uid，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── 签到（仅 CN：POST codebuddy.cn/v2/billing/meter/daily-checkin，幂等不阻塞）───
# 国际站没有 CN 那套成长/签到体系（上游一律 400 code 10001），发起只会写一条
# 误导性错误，故按站点跳过。
# OAuth 返回字段一律通过环境变量传入 Python，并用带引号 heredoc：
# 昵称/domain/token 等值可含引号或换行，拼进 Python 源码会造成注入。
if [[ "$SITE" == "cn" ]]; then
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
python3 - <<'PYEOF'
import json, os, urllib.request, urllib.error

token = os.environ["WB2A_LOGIN_TOKEN"]
user_id = os.environ["WB2A_LOGIN_USER_ID"]
ent_id = os.environ["WB2A_LOGIN_ENT_ID"]
domain = os.environ["WB2A_LOGIN_DOMAIN"]

req = urllib.request.Request(
    "https://www.codebuddy.cn/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}",
    headers={
        "Authorization": "Bearer " + token,
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-User-Id": user_id,
        **({"X-Enterprise-Id": ent_id, "X-Tenant-Id": ent_id} if ent_id else {}),
        **({"X-Domain": domain} if domain else {}),
    })
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"签到: 成功 {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"签到: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"签到: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"签到: http {e.code}")
except Exception as e:
    print(f"签到: {e}")
PYEOF
else
    echo "签到: 跳过（国际站无 CN 签到体系）"
fi

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=${USER_ID}），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=${USER_ID}），新增 auth 文件"
    ACTION="新增"
fi
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_REFRESH="$REFRESH" \
WB2A_LOGIN_EXPIRES_AT="$EXPIRES_AT" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
WB2A_LOGIN_REALM="$([[ "$SITE" == "cn" ]] && echo cn || echo global)" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_NICKNAME="$NICKNAME" \
WB2A_LOGIN_AUTH_FILE="$AUTH_FILE" \
WB2A_LOGIN_ACTION="$ACTION" \
python3 - <<'PYEOF'
import json, os

auth = {
    "account": {
        "uid": os.environ["WB2A_LOGIN_USER_ID"],
        "enterpriseId": os.environ["WB2A_LOGIN_ENT_ID"],
        "nickname": os.environ["WB2A_LOGIN_NICKNAME"],
    },
    "auth": {
        "accessToken": os.environ["WB2A_LOGIN_TOKEN"],
        "refreshToken": os.environ["WB2A_LOGIN_REFRESH"],
        "expiresAt": int(os.environ["WB2A_LOGIN_EXPIRES_AT"]),
        "domain": os.environ["WB2A_LOGIN_DOMAIN"],
        # 显式写 realm：国际站有两个主机名（codebuddy.ai / workbuddy.ai），
        # 只靠 domain 后缀推断会让「未来新增域」静默落到 cn，写死更稳。
        "realm": os.environ["WB2A_LOGIN_REALM"],
    },
}
with open(os.environ["WB2A_LOGIN_AUTH_FILE"], "w") as f:
    json.dump(auth, f, indent=1)
print(f"已保存（{os.environ['WB2A_LOGIN_ACTION']}）: {os.environ['WB2A_LOGIN_AUTH_FILE']}")
PYEOF

# ─── 重启服务 ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    # API_KEY 从 config.json 读取（该变量在脚本中未定义，fallback 仅为占位，不会通过鉴权）
    API_KEY=$(python3 -c "import json; print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
    COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-test_key}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  站点: $(site_label "$SITE")"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Token: ${TOKEN:0:30}..."
echo "  有效期: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
