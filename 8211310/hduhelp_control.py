"""
对照实验：判断上面的 404 到底说明什么。
如果连随机乱码子域名都返回同样的 404，那说明是通配 DNS + 兜底服务器，
"404" 就不能证明接口不存在，只能证明这台机器上没配这个路由。
"""
import socket
import ssl

import requests

CONTROL_HOSTS = [
    "https://api.hduhelp.com",
    "https://openapi.hduhelp.com",
    "https://zzq-nonexistent-8f3k2.hduhelp.com",   # 故意编造的乱码子域名
    "https://hduhelp.com",
    "https://open.hduhelp.com",                    # 文档站，已知是活的
    "https://cinnamon.hduhelp.com",                # 头像 CDN，文档里出现过
]

CONTROL_PATH = "/zzq-definitely-not-a-real-path-9x7q"


def resolve(host):
    try:
        return sorted({i[4][0] for i in socket.getaddrinfo(host, 443, proto=socket.IPPROTO_TCP)})
    except socket.gaierror as e:
        return f"DNS 失败: {e}"


def cert_info(host):
    try:
        ctx = ssl.create_default_context()
        with socket.create_connection((host, 443), timeout=8) as sock:
            with ctx.wrap_socket(sock, server_hostname=host) as ssock:
                cert = ssock.getpeercert()
                subj = dict(x[0] for x in cert.get("subject", ()))
                san = [v for t, v in cert.get("subjectAltName", ()) if t == "DNS"]
                return subj.get("commonName"), san
    except Exception as e:
        return f"ERR {type(e).__name__}", str(e)[:50]


print("=" * 80)
print("对照实验 A：各域名的 DNS / 证书")
print("=" * 80)
for base in CONTROL_HOSTS:
    host = base.split("//")[1]
    cn, san = cert_info(host)
    print(f"\n{host}")
    print(f"  IP     : {resolve(host)}")
    print(f"  证书CN : {cn}")
    print(f"  SAN    : {san}")

print("\n" + "=" * 80)
print("对照实验 B：已知站点 vs 乱码站点，请求同一个乱码路径")
print("=" * 80)
print(f"{'HOST':<40} {'STATUS':<9} BODY")
print("-" * 80)
for base in CONTROL_HOSTS:
    try:
        r = requests.get(base + CONTROL_PATH, timeout=8, allow_redirects=False)
        body = r.text.strip().replace("\n", " ")[:60]
        print(f"{base:<40} {r.status_code:<9} {body}")
    except Exception as e:
        print(f"{base:<40} {type(e).__name__:<9} {str(e)[:60]}")

print("\n" + "=" * 80)
print("对照实验 C：文档站 open.hduhelp.com 的正常页面（证明 requests 本身没问题）")
print("=" * 80)
for p in ["/", "/docs/develop/index.html"]:
    try:
        r = requests.get("https://open.hduhelp.com" + p, timeout=10)
        print(f"{p:<32} {r.status_code:<6} {len(r.text)} 字符  ct={r.headers.get('content-type')}")
    except Exception as e:
        print(f"{p:<32} {type(e).__name__}: {e}")
