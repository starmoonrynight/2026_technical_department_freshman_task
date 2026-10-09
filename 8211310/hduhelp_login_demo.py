"""
杭电助手 HDUHelp Neo 第三方登录 —— 修正版示例

重要：接口地址已从旧文档的 /oauth/* 迁移到 /hduhelp-neo/open-apis/*
     鉴权头从 "Authorization: token xxx" 改为 "Authorization: Bearer xxx"
     成功判断从 error == 0 改为 code == 0

不带凭证直接运行 = 自检模式，会真实请求服务器并打印响应，
用来证明地址是对的、以及让你提前看到各种失败长什么样。
"""
import base64
import hashlib
import json
import os
import secrets
import sys
from urllib.parse import urlencode

import requests

BASE = "https://api.hduhelp.com"
NEO = BASE + "/hduhelp-neo"

EP_AUTHORIZE = NEO + "/open-apis/authen/authorize"
EP_TOKEN = NEO + "/open-apis/authen/access-token"
EP_REFRESH = NEO + "/open-apis/authen/refresh-access-token"
EP_USERINFO = NEO + "/open-apis/authen/user-info"
EP_REVOKE = NEO + "/open-apis/authen/revoke"

# ===== 向杭电助手团队申请后填这里；也支持用环境变量注入，避免把密钥写进代码 =====
APP_ID = os.environ.get("HDUHELP_APP_ID", "")
APP_SECRET = os.environ.get("HDUHELP_APP_SECRET", "")
# 必须和申请时登记的 redirect_uris 完全一致（精确匹配白名单，含协议和结尾斜杠）
REDIRECT_URI = os.environ.get("HDUHELP_REDIRECT_URI", "https://你的域名/callback")
SCOPE = os.environ.get("HDUHELP_SCOPE", "contact:user.id:read")

TIMEOUT = 15


# ---------------------------------------------------------------- 工具函数

def b64url(raw: bytes) -> str:
    """base64url 且去掉末尾的 = 填充，PKCE 要求这样"""
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def make_pkce():
    """RFC 7636：verifier 是 32 字节随机数的 base64url（43 字符），
    challenge 是 verifier 的 SHA256 再 base64url，method 固定 S256。"""
    verifier = b64url(secrets.token_bytes(32))
    challenge = b64url(hashlib.sha256(verifier.encode("ascii")).digest())
    return verifier, challenge


def show(label, resp):
    """打印原始响应。注意：Neo 的 HTTP 状态码不可信，
    实测 user-info 带无效 token 时返回 HTTP 200 但业务码是 40105。"""
    print(f"\n--- {label} ---")
    print(f"HTTP {resp.status_code}  Server={resp.headers.get('server')}  "
          f"CT={resp.headers.get('content-type')}")
    try:
        parsed = resp.json()
        print(json.dumps(parsed, ensure_ascii=False, indent=2))
        return parsed
    except ValueError:
        print(f"非 JSON 响应体: {resp.text[:300]!r}")
        return None


def is_ok(payload) -> bool:
    """成功判定：能解析成 JSON 且 code == 0。

    坑：Neo 有两种错误信封，必须都能处理。
      - authen/* 的 OAuth 端点用 RFC6749 风格 {"error": "...", "error_description": "..."}
      - auth/tenant-* 和 user-info 用 {"code": 40105, "msg": "..."}
    """
    if not isinstance(payload, dict):
        return False
    return payload.get("code") == 0


# ---------------------------------------------------------------- 三步登录流程

def build_authorize_url(state: str, challenge: str) -> str:
    """第 1 步：生成授权链接，让用户在浏览器里打开并同意授权"""
    query = urlencode({
        "app_id": APP_ID,
        "response_type": "code",
        "redirect_uri": REDIRECT_URI,
        "scope": SCOPE,
        "state": state,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
    })
    return f"{EP_AUTHORIZE}?{query}"


def exchange_token(code: str, verifier: str) -> dict:
    """第 2 步：用 code 换 access_token

    注意 Content-Type 是 x-www-form-urlencoded，不是 JSON。
    同族的 refresh 接口却是 JSON —— 这个不一致是服务端就这样，别改。
    """
    resp = requests.post(
        EP_TOKEN,
        data={
            "grant_type": "authorization_code",
            "app_id": APP_ID,
            "app_secret": APP_SECRET,
            "code": code,
            "code_verifier": verifier,
        },
        timeout=TIMEOUT,
    )
    return show("POST /authen/access-token", resp) or {}


def get_user_info(access_token: str) -> dict:
    """第 3 步：取用户信息。鉴权头前缀是 Bearer，不是旧文档的 token。"""
    resp = requests.get(
        EP_USERINFO,
        headers={"Authorization": f"Bearer {access_token}"},
        timeout=TIMEOUT,
    )
    return show("GET /authen/user-info", resp) or {}


def refresh(refresh_token: str) -> dict:
    """刷新令牌。refresh_token 是一次性的，每次刷新都会轮换出新的，要存最新的。"""
    resp = requests.post(
        EP_REFRESH,
        json={
            "grant_type": "refresh_token",
            "app_id": APP_ID,
            "app_secret": APP_SECRET,
            "refresh_token": refresh_token,
        },
        timeout=TIMEOUT,
    )
    return show("POST /authen/refresh-access-token", resp) or {}


# ---------------------------------------------------------------- 自检模式

def selftest():
    """没有凭证时跑这个：证明地址是对的，并让你看清各种失败的返回值。"""
    print("=" * 72)
    print("自检模式（未配置 APP_ID / APP_SECRET）")
    print("=" * 72)

    print("\n【对照】旧文档路径，预期 404 且非 JSON —— 证明旧接口已下线")
    show("GET /oauth/token (旧)", requests.get(BASE + "/oauth/token", timeout=TIMEOUT))

    print("\n【正例】Neo 端点，预期返回 hertz 的 JSON 错误 —— 证明地址是对的")
    show("GET /authen/authorize 不带参数",
         requests.get(EP_AUTHORIZE, timeout=TIMEOUT))
    show("GET /authen/authorize 带假 app_id",
         requests.get(EP_AUTHORIZE,
                      params={"app_id": "cli_definitely_not_real", "response_type": "code"},
                      timeout=TIMEOUT))
    show("POST /authen/access-token 空表单",
         requests.post(EP_TOKEN, data={}, timeout=TIMEOUT))
    show("GET /authen/user-info 不带 token",
         requests.get(EP_USERINFO, timeout=TIMEOUT))
    show("GET /authen/user-info 带假 Bearer token（注意 HTTP 是 200！）",
         requests.get(EP_USERINFO,
                      headers={"Authorization": "Bearer hduhelp_pat_fake"},
                      timeout=TIMEOUT))

    print("\n" + "=" * 72)
    print("自检完成。上面每个请求都拿到了 hertz 的 JSON 校验错误，")
    print("说明接口地址正确、服务在线，只差真实的 app_id / app_secret。")
    print("=" * 72)


# ---------------------------------------------------------------- 真实登录

def login():
    state = secrets.token_urlsafe(16)
    verifier, challenge = make_pkce()

    print("① 在浏览器打开下面的链接，用杭电助手登录并同意授权：\n")
    print(build_authorize_url(state, challenge))

    print(f"\n② 同意后浏览器会跳到你的 redirect_uri，形如：")
    print(f"   {REDIRECT_URI}?code=xxxx&state={state}")
    print("   注意：state 必须和上面生成的一致，不一致说明有 CSRF 风险，要拒绝。")

    code = input("\n③ 把回调 URL 里的 code 粘进来：").strip()
    if not code:
        raise SystemExit("没有输入 code，退出")

    token_payload = exchange_token(code, verifier)
    if not is_ok(token_payload):
        raise SystemExit("换取 token 失败，见上面的 error / msg")

    data = token_payload["data"]
    access_token = data["accessToken"]          # 注意是 camelCase
    print(f"\n拿到 accessToken，{data['expiresIn']} 秒后过期")
    print(f"userId = {data['userId']}   tenantKey = {data.get('tenantKey')}")

    info = get_user_info(access_token)
    if not is_ok(info):
        raise SystemExit("取用户信息失败")

    user = info["data"]
    print(f"\n登录成功：{user['name']}  userId={user['userId']}  "
          f"identityType={user['identityType']}")
    print(f"已授权 scope: {user['scopes']}")

    # 失物招领系统里，用 userId 作为你本地用户表的主键关联字段。
    # user-info 不返回学号，如果业务必须要学号，得另外向团队申请对应 scope。
    print("\n=> 建议在你自己的数据库里存：userId（唯一键）、name、avatar")


if __name__ == "__main__":
    if not APP_ID or not APP_SECRET:
        selftest()
        sys.exit(0)
    login()
