#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
stub Genet —— 内置 deploy/clean Runner 冒烟测试用的假 Genet OpenAPI（stdlib，无三方依赖）。

响应形状对齐 tools/fake-genet（server.go）与 docs/research/genet-lifecycle.md 的接口表：
  POST   /api/open/helm/uc-packages/{stack|pd}/releases → 202 {name, namespace, status, ...}
         同名再 POST → 400 {"message": "release ... already has protected metadata; ..."}（§9.2）
  GET    /api/open/helm/releases/{name}[?light=true]    → 200 {status, callInfo.endpoint, pods, ...} / 404
  GET    /api/open/helm/releases/{name}/pods            → 200 {items:[...]}（已删后仍可查，返回空）
  DELETE /api/open/helm/releases/{name}?keepHistory=&force= → 200 {message} / 404
  GET    /api/open/queue                                → 200 {enabled, queue:{state,pending,inqueue,running}}
  GET    /__counters                                    → 各端点命中计数（测试断言用，免鉴权）

鉴权：/api/open/* 需要 `Authorization: Bearer <--token>`，否则 401。
命名空间：API Key 绑定语义用 --namespace 固定（请求体不能指定）。

场景开关（可组合）：
  --ready-after SEC        安装后 SEC 秒才 deployed+endpoint ready（默认 0）
  --fail-after SEC         安装后 SEC 秒 release 变 failed（blocked 路径）
  --endpoint-blocked       deployed 但 endpoint 永远 blocked（配额不足模拟）
  --delayed-visible SEC    前 SEC 秒内 GET 404（release 异步 apply 未可见；POST 一律 400 protected）
  --delete-pending SEC     DELETE 后 SEC 秒 release 才真正消失（删除后验证循环）
  --pods-500               /pods 端点恒 500（R9：查询失败 ≠ 为空）
  --preinstall NAME        启动时预置一个已就绪 release（恢复场景 / clean 需删除场景）

绑定 0.0.0.0（容器内经 host.docker.internal 访问宿主上的 stub），端口 0 时随机分配，
绑定后向 stdout 打印 `STUB_PORT=<port>`（测试用该行取端口）。
"""
import argparse
import json
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REL_RE = re.compile(r"^[a-z][a-z0-9-]{0,52}$")
NAMESPACE_DEFAULT = "argus-svc"


class Store:
    """release 状态机：pending-install → (deployed | failed)；DELETE 可带 pending 窗口。"""

    def __init__(self, args):
        self.args = args
        self.lock = threading.Lock()
        self.releases = {}   # name -> dict(created_at, ready_at, fail_at, delete_at)
        self.counters = {"get_release": 0, "post_install": 0, "delete": 0, "get_pods": 0, "get_queue": 0}
        for name in args.preinstall or []:
            self.releases[name] = {
                "created_at": time.time(),
                "ready_at": time.time(),
                "fail_at": None,
                "delete_at": None,
            }

    def effective(self, name, now=None):
        """返回 (info|None)：DELETE pending 未到期视为仍存在；delayed_visible 未到期视为不可见。"""
        now = now if now is not None else time.time()
        rel = self.releases.get(name)
        if rel is None:
            return None
        if rel["delete_at"] is not None and now >= rel["delete_at"]:
            del self.releases[name]
            return None
        if self.args.delayed_visible > 0 and now - rel["created_at"] < self.args.delayed_visible:
            return None  # release 尚未 apply 出来（GET 404，POST 400 protected）
        if rel["fail_at"] is not None:
            # --fail-after 场景：安装注定失败，失败前一直是 pending-install
            # （否则 deployed+ready 窗口内 runner 会先判 ready）。
            status = "failed" if now >= rel["fail_at"] else "pending-install"
        elif now < rel["ready_at"]:
            status = "pending-install"
        else:
            status = "deployed"
        return {"name": name, "status": status, "rel": rel}

    def pods(self, info):
        name, status = info["name"], info["status"]
        if status == "deployed":
            return [{"name": f"{name}-mock-llm-0", "phase": "Running",
                     "nodeName": "stub-node-a", "ready": True}]
        phase = "Failed" if status == "failed" else "Pending"
        return [{"name": f"{name}-mock-llm-0", "phase": phase, "nodeName": "", "ready": False}]

    def call_info(self, info):
        name, status = info["name"], info["status"]
        svc = f"{name}-mock-llm"
        if self.args.endpoint_blocked:
            return {"namespace": self.args.namespace, "model": self.args.model, "serviceName": svc,
                    "endpoint": {"status": "blocked", "source": "service",
                                 "message": "insufficient quota (stub)"}}
        if status == "failed":
            return {"namespace": self.args.namespace, "model": self.args.model, "serviceName": svc,
                    "endpoint": {"status": "blocked", "source": "service",
                                 "message": "install chart timeout (stub)"}}
        if status == "deployed":
            return {"namespace": self.args.namespace, "model": self.args.model, "serviceName": svc,
                    "endpoint": {"status": "ready", "source": "service", "model": self.args.model,
                                 "url": f"http://{svc}.{self.args.namespace}.svc:18080/v1",
                                 "host": f"{svc}.{self.args.namespace}.svc", "port": 18080,
                                 "message": ""}}
        return {"namespace": self.args.namespace, "model": self.args.model, "serviceName": svc,
                "endpoint": {"status": "pending", "source": "service",
                             "message": "waiting for pods to become ready"}}


class Handler(BaseHTTPRequestHandler):
    store: Store = None  # 由 make_server 注入

    # ---- 基础设施 -----------------------------------------------------------
    def log_message(self, fmt, *args):  # 静默：测试输出只留断言
        pass

    def _json(self, code, payload):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authed(self):
        got = (self.headers.get("Authorization") or "").strip()
        want = "Bearer " + self.store.args.token
        return got == want

    def _body_json(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0:
            return {}
        try:
            return json.loads(self.rfile.read(n).decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            return None

    # ---- 路由 ---------------------------------------------------------------
    def do_GET(self):
        s = self.store
        path = self.path.split("?", 1)[0]
        if path == "/__counters":
            with s.lock:
                return self._json(200, {"counters": dict(s.counters),
                                        "releases": sorted(s.releases.keys())})
        if path == "/api/open/queue":
            with s.lock:
                s.counters["get_queue"] += 1
            if not self._authed():
                return self._json(401, {"message": "invalid or missing API key"})
            return self._json(200, {"enabled": True, "schedulerName": "volcano",
                                    "queue": {"state": "Pending", "pending": 3,
                                              "inqueue": 1, "running": 2}})
        m = re.fullmatch(r"/api/open/helm/releases/([^/]+)/pods", path)
        if m:
            with s.lock:
                s.counters["get_pods"] += 1
            if not self._authed():
                return self._json(401, {"message": "invalid or missing API key"})
            if self.store.args.pods_500:
                return self._json(500, {"message": "simulated pods query failure (R9)"})
            info = s.effective(m.group(1))
            items = s.pods(info) if info else []
            return self._json(200, {"items": items})
        m = re.fullmatch(r"/api/open/helm/releases/([^/]+)", path)
        if m:
            with s.lock:
                s.counters["get_release"] += 1
            if not self._authed():
                return self._json(401, {"message": "invalid or missing API key"})
            info = s.effective(m.group(1))
            if info is None:
                return self._json(404, {"message": "helm release not found"})
            return self._json(200, {
                "name": info["name"], "requestedName": info["name"],
                "namespace": s.args.namespace, "packageType": "stack",
                "chart": "mock-llm", "chartVersion": "0.1.0", "revision": 1,
                "status": info["status"],
                "description": ("install chart timeout (stub)"
                                if info["status"] == "failed" else "stub release"),
                "pods": s.pods(info), "callInfo": s.call_info(info),
            })
        return self._json(404, {"message": "unknown path"})

    def do_POST(self):
        s = self.store
        path = self.path.split("?", 1)[0]
        m = re.fullmatch(r"/api/open/helm/uc-packages/([^/]+)/releases", path)
        if not m:
            return self._json(404, {"message": "unknown path"})
        with s.lock:
            s.counters["post_install"] += 1
        if not self._authed():
            return self._json(401, {"message": "invalid or missing API key"})
        package_type = m.group(1)
        if package_type not in ("stack", "pd"):
            return self._json(400, {"message": f"invalid package type {package_type!r}"})
        body = self._body_json()
        if body is None:
            return self._json(400, {"message": "invalid JSON body"})
        name = str(body.get("releaseName") or "")
        if not name:
            return self._json(400, {"message": "releaseName is required"})
        if not REL_RE.match(name):
            return self._json(400, {"message": f"releaseName {name!r} must match {REL_RE.pattern}"})
        values = body.get("values")
        template_path = body.get("templatePath")
        template_id = body.get("templateId")
        if not (values or template_path or template_id):
            return self._json(400, {"message": "templatePath 或 values 不能为空"})
        now = time.time()
        with s.lock:
            if s.effective(name, now) is not None or name in s.releases:
                return self._json(400, {"message":
                    f"release {name!r} already has protected metadata; "
                    "install cannot replace an existing release"})
            s.releases[name] = {
                "created_at": now,
                "ready_at": now + max(0.0, s.args.ready_after),
                "fail_at": (now + s.args.fail_after) if s.args.fail_after else None,
                "delete_at": None,
            }
        return self._json(202, {"name": name, "requestedName": name,
                                "namespace": s.args.namespace, "packageType": package_type,
                                "chart": "mock-llm", "chartVersion": "0.1.0",
                                "status": "pending-install"})

    def do_DELETE(self):
        s = self.store
        path = self.path.split("?", 1)[0]
        m = re.fullmatch(r"/api/open/helm/releases/([^/]+)", path)
        if not m:
            return self._json(404, {"message": "unknown path"})
        with s.lock:
            s.counters["delete"] += 1
        if not self._authed():
            return self._json(401, {"message": "invalid or missing API key"})
        name = m.group(1)
        now = time.time()
        with s.lock:
            if s.effective(name, now) is None:
                return self._json(404, {"message": "helm release not found"})
            delay = max(0.0, s.args.delete_pending)
            if delay > 0:
                s.releases[name]["delete_at"] = now + delay  # 仍可见，稍后消失（验证循环用）
            else:
                del s.releases[name]
        return self._json(200, {"message": f"release {name!r} uninstalled (stub)",
                                "forcedPods": [], "secretsDeleted": 0, "cleanupErrors": []})


def make_server(args):
    Handler.store = Store(args)
    return ThreadingHTTPServer(("0.0.0.0", args.port), Handler)


def main():
    ap = argparse.ArgumentParser(description="stub Genet for builtin runner smoke tests")
    ap.add_argument("--port", type=int, default=0)
    ap.add_argument("--token", default="stub-genet-key")
    ap.add_argument("--namespace", default=NAMESPACE_DEFAULT)
    ap.add_argument("--model", default="Qwen3-32B")
    ap.add_argument("--ready-after", type=float, default=0.0)
    ap.add_argument("--fail-after", type=float, default=0.0)
    ap.add_argument("--endpoint-blocked", action="store_true")
    ap.add_argument("--delayed-visible", type=float, default=0.0)
    ap.add_argument("--delete-pending", type=float, default=0.0)
    ap.add_argument("--pods-500", action="store_true")
    ap.add_argument("--preinstall", action="append", default=[])
    args = ap.parse_args()

    srv = make_server(args)
    port = srv.server_address[1]
    print(f"STUB_PORT={port}", flush=True)   # 测试据此取端口
    print(f"stub-genet: listening on 0.0.0.0:{port} ns={args.namespace} "
          f"ready-after={args.ready_after} fail-after={args.fail_after} "
          f"endpoint-blocked={args.endpoint_blocked} delayed-visible={args.delayed_visible} "
          f"delete-pending={args.delete_pending} pods-500={args.pods_500} "
          f"preinstall={args.preinstall}", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
