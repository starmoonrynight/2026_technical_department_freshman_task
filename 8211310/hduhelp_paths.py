"""
路径探测：搞清楚 api.hduhelp.com 上到底有哪些前缀是活的。
背景：文档写的 /oauth/token 返回 404，但 SDK 仓库里出现过 /campusapis/staff/v1/... 这样的路径。
每个路径只发一次请求，短超时，不重试。
"""
import requests

HOSTS = [
    "https://api.hduhelp.com",
    "https://openapi.hduhelp.com",
]

PATHS = [
    "/",
    "/health",
    "/ping",
    "/version",
    # 文档里写的 oauth 路径
    "/oauth/token",
    "/oauth/authorize",
    # 可能的 oauth 变体
    "/login/oauth/token",
    "/login/oauth/authorize",
    "/api/oauth/token",
    "/v1/oauth/token",
    "/open/oauth/token",
    # SDK 里出现过的前缀
    "/campusapis/",
    "/campusapis/staff/v1/freshman",
    # 迁移说明里提到的 base / salmon_base
    "/base/",
    "/salmon_base/",
    "/salmon_base/person/info",
    "/salmon_base/student/info",
    # 文档里写的 user 接口
    "/user/get",
    "/user/info",
    # 其他业务接口（文档目录里出现过的）
    "/time/time",
    "/logistics/card",
]


def probe(base, path):
    url = base + path
    try:
        resp = requests.get(url, timeout=8, allow_redirects=False)
        body = resp.text.strip().replace("\n", " ")
        if len(body) > 90:
            body = body[:90] + "..."
        return resp.status_code, body
    except requests.exceptions.SSLError as e:
        return "SSL_ERR", str(e)[:60]
    except requests.exceptions.ConnectionError as e:
        return "CONN_ERR", str(e)[:60]
    except requests.exceptions.Timeout:
        return "TIMEOUT", ""
    except Exception as e:
        return type(e).__name__, str(e)[:60]


for base in HOSTS:
    print("\n" + "#" * 78)
    print(f"# {base}")
    print("#" * 78)
    print(f"{'PATH':<38} {'STATUS':<10} BODY")
    print("-" * 78)
    for p in PATHS:
        status, body = probe(base, p)
        print(f"{p:<38} {str(status):<10} {body}")
