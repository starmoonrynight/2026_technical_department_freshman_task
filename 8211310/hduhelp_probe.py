"""
探测脚本：用占位凭证真实请求杭电助手 OPENAPI，把原始响应打出来。
目的：验证文档里的接口是否可达，以及失败时到底返回什么（文档没写错误码表）。
只发少量请求，不做任何暴力尝试。
"""
import socket
import ssl

import requests

BASE = "https://api.hduhelp.com"

FAKE_CLIENT_ID = "probe_client_id_0000"
FAKE_CLIENT_SECRET = "probe_client_secret_0000"
FAKE_REDIRECT = "https://example.com/callback"
FAKE_CODE = "probe_code_0000"
FAKE_STATE = "probe_state_0000"


def banner(title):
    print("\n" + "=" * 70)
    print(title)
    print("=" * 70)


def dump(resp, body_limit=1200):
    print(f"HTTP {resp.status_code} {resp.reason}")
    print("-- 响应头 --")
    for k in ("content-type", "location", "www-authenticate", "server"):
        if k in resp.headers:
            print(f"  {k}: {resp.headers[k]}")
    print("-- 响应体 --")
    text = resp.text
    print(text[:body_limit] if text.strip() else "(空)")
    if len(text) > body_limit:
        print(f"... (已截断，共 {len(text)} 字符)")


def step_dns_tls():
    banner("步骤 0：DNS 解析 + TLS 握手")
    try:
        infos = socket.getaddrinfo("api.hduhelp.com", 443, proto=socket.IPPROTO_TCP)
        ips = sorted({i[4][0] for i in infos})
        print(f"DNS 解析成功: api.hduhelp.com -> {ips}")
    except socket.gaierror as e:
        print(f"DNS 解析失败: {e}")
        return False

    try:
        ctx = ssl.create_default_context()
        with socket.create_connection(("api.hduhelp.com", 443), timeout=10) as sock:
            with ctx.wrap_socket(sock, server_hostname="api.hduhelp.com") as ssock:
                cert = ssock.getpeercert()
                print(f"TLS 握手成功: {ssock.version()}")
                subj = dict(x[0] for x in cert.get("subject", ()))
                print(f"证书 CN: {subj.get('commonName')}")
                print(f"证书有效期止: {cert.get('notAfter')}")
        return True
    except Exception as e:
        print(f"TLS 握手失败: {type(e).__name__}: {e}")
        return False


def step_root():
    banner("步骤 1：GET /  （确认服务活着）")
    try:
        dump(requests.get(BASE + "/", timeout=15, allow_redirects=False))
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


def step_authorize():
    banner("步骤 2：GET /oauth/authorize （假 client_id，不跟随跳转）")
    try:
        resp = requests.get(
            f"{BASE}/oauth/authorize",
            params={
                "response_type": "code",
                "client_id": FAKE_CLIENT_ID,
                "redirect_uri": FAKE_REDIRECT,
                "state": FAKE_STATE,
            },
            timeout=15,
            allow_redirects=False,
        )
        dump(resp)
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


def step_token():
    banner("步骤 3：GET /oauth/token （假凭证 + 假 code）")
    try:
        resp = requests.get(
            f"{BASE}/oauth/token",
            params={
                "client_id": FAKE_CLIENT_ID,
                "client_secret": FAKE_CLIENT_SECRET,
                "grant_type": "authorization_code",
                "code": FAKE_CODE,
                "state": FAKE_STATE,
            },
            timeout=15,
        )
        dump(resp)
        print("-- 能否按文档的 error==0 判断 --")
        try:
            print(f"  resp.json() 解析成功 -> {resp.json()}")
        except ValueError as e:
            print(f"  不是 JSON！{type(e).__name__}: {e}")
            print("  => 说明我上一版脚本里的 token_resp.get('error') 会直接抛异常")
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


def step_token_post():
    banner("步骤 4：POST /oauth/token （验证文档说的 GET 是否唯一可行）")
    try:
        resp = requests.post(
            f"{BASE}/oauth/token",
            data={
                "client_id": FAKE_CLIENT_ID,
                "client_secret": FAKE_CLIENT_SECRET,
                "grant_type": "authorization_code",
                "code": FAKE_CODE,
                "state": FAKE_STATE,
            },
            timeout=15,
        )
        dump(resp, body_limit=600)
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


def step_user_no_token():
    banner("步骤 5：GET /user/get （不带 Authorization 头）")
    try:
        dump(requests.get(f"{BASE}/user/get", timeout=15), body_limit=600)
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


def step_user_bad_token():
    banner("步骤 6：GET /user/get （带假的 'token xxx' 头）")
    try:
        resp = requests.get(
            f"{BASE}/user/get",
            headers={"Authorization": "token fake_access_token_0000"},
            timeout=15,
        )
        dump(resp, body_limit=600)
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


def step_user_bearer():
    banner("步骤 7：GET /user/get （对比 'Bearer xxx' 头，验证前缀是否必须是 token）")
    try:
        resp = requests.get(
            f"{BASE}/user/get",
            headers={"Authorization": "Bearer fake_access_token_0000"},
            timeout=15,
        )
        dump(resp, body_limit=600)
    except Exception as e:
        print(f"请求异常: {type(e).__name__}: {e}")


if __name__ == "__main__":
    if not step_dns_tls():
        print("\n网络层就不通，后面的 HTTP 探测没有意义，提前结束。")
        raise SystemExit(1)

    step_root()
    step_authorize()
    step_token()
    step_token_post()
    step_user_no_token()
    step_user_bad_token()
    step_user_bearer()

    banner("探测结束")
