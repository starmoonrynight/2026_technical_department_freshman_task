"""
验证脚本：确认 HDUHelp Neo 的真实接口是否存在、以及失败时到底返回什么。
不发任何真实凭证，只发空/占位参数，看服务端的参数校验错误。
"""
import requests

BASE = "https://api.hduhelp.com"
NEO = BASE + "/hduhelp-neo"

CHECKS = [
    ("旧文档路径 GET /oauth/token", "GET", BASE + "/oauth/token", None, None),
    ("旧文档路径 GET /user/get", "GET", BASE + "/user/get", None, None),
    ("Neo 健康检查 GET /hduhelp-neo/health/config", "GET", NEO + "/health/config", None, None),
    ("Neo 授权页 GET /open-apis/authen/authorize (无参数)", "GET", NEO + "/open-apis/authen/authorize", None, None),
    ("Neo 授权页 GET /open-apis/authen/authorize (假 app_id)", "GET",
     NEO + "/open-apis/authen/authorize", {"app_id": "cli_zzq_probe", "response_type": "code"}, None),
    ("Neo 换token POST /open-apis/authen/access-token (空表单)", "POST",
     NEO + "/open-apis/authen/access-token", None, {}),
    ("Neo 用户信息 GET /open-apis/authen/user-info (无token)", "GET",
     NEO + "/open-apis/authen/user-info", None, None),
    ("Neo 用户信息 GET /open-apis/authen/user-info (假token)", "GET",
     NEO + "/open-apis/authen/user-info", None, None),
    ("Neo 租户token POST /open-apis/auth/tenant-access-token/internal (空)", "POST",
     NEO + "/open-apis/auth/tenant-access-token/internal", None, {}),
    ("Neo 设备码 POST /open-apis/auth/device-authorization (空表单)", "POST",
     NEO + "/open-apis/auth/device-authorization", None, {}),
]

FAKE_TOKEN_HEADERS = {"Authorization": "Bearer hduhelp_pat_zzq_not_a_real_token"}


def run(name, method, url, params, form):
    print("\n" + "=" * 78)
    print(name)
    print(f"{method} {url}")
    print("=" * 78)
    try:
        if method == "GET":
            headers = FAKE_TOKEN_HEADERS if "假token" in name else None
            r = requests.get(url, params=params, headers=headers, timeout=12, allow_redirects=False)
        else:
            r = requests.post(url, data=form, timeout=12, allow_redirects=False)

        print(f"HTTP {r.status_code}   Server={r.headers.get('server')!r}   "
              f"Content-Type={r.headers.get('content-type')!r}")
        body = r.text.strip()
        print(body[:500] if body else "(空响应体)")
        try:
            r.json()
            print("  -> 是合法 JSON")
        except ValueError:
            print("  -> 不是 JSON")
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


for args in CHECKS:
    run(*args)
