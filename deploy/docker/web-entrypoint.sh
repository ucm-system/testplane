#!/bin/sh
# Argus 前端镜像入口：渲染 nginx 模板 → exec nginx。
#
# 为什么需要它（而不是官方镜像的 /docker-entrypoint.d 机制）：
#   1. 官方脚本用 envsubst 作用在整份模板上，/api 反代那个 location 需要**整块出现或整块消失**
#      （nginx 不接受空的 proxy_pass），官方机制没有条件段落；
#   2. 只读根 + 非 root 时，官方脚本还会尝试改写 /etc/nginx/conf.d（属主 10001 可写，但空挂
#      覆盖后不可写），显式自管更可控。
#
# 支持的变量：
#   AG_API_UPSTREAM   可选。/api/ 的反代上游（如 http://argus-api:8080）。
#                            不设 = 纯静态（/api/ 会落到 SPA fallback）。
#   AG_NGINX_RESOLVER 可选。上游是域名且想动态解析时用（如 127.0.0.11）。
set -eu

TEMPLATE=/etc/nginx/templates/default.conf.template
# 渲染结果必须写到可写位置：只读根（readOnlyRootFilesystem）下 /etc 不可写，
# /tmp 由编排挂成 emptyDir（k8s）或 --tmpfs（docker run），是唯一稳的落点。
OUT=/tmp/nginx/conf.d/default.conf
RUNTIME=/tmp/nginx-site.conf

if [ ! -f "$TEMPLATE" ]; then
    echo "web-entrypoint: 模板不存在：$TEMPLATE" >&2
    exit 1
fi

# 只读根：/tmp 是唯一保证可写的地方（k8s 里是 emptyDir，docker 里是 --tmpfs）。
# 起不来时这里会明确报错，而不是留个"起来但 502"的容器。
mkdir -p /tmp/nginx/conf.d /tmp/client_body /tmp/proxy /var/cache/nginx 2>/dev/null || true

# 1) 按上游配置决定 /api/ 反代块是否保留
if [ -n "${AG_API_UPSTREAM:-}" ]; then
    # 上游 URL 必须带 scheme，nginx 的 proxy_pass 才能直接解析
    case "$AG_API_UPSTREAM" in
        http://*|https://*) : ;;
        *) echo "web-entrypoint: AG_API_UPSTREAM 必须以 http:// 或 https:// 开头：$AG_API_UPSTREAM" >&2; exit 1 ;;
    esac
    # 去掉上游末尾的 /，否则 proxy_pass 会吃掉 /api/ 前缀的拼接
    export AG_API_UPSTREAM="${AG_API_UPSTREAM%/}"
    # 保留 BEGIN/END 之间的块
    sed -e '/BEGIN_API_PROXY/,/END_API_PROXY/{
        /BEGIN_API_PROXY/d
        /END_API_PROXY/d
    }' "$TEMPLATE" > "$RUNTIME"
    echo "web-entrypoint: /api/ 反代 -> $AG_API_UPSTREAM" >&2
else
    # 纯静态：删掉整个块（连同两行标记）
    sed -e '/BEGIN_API_PROXY/,/END_API_PROXY/d' "$TEMPLATE" > "$RUNTIME"
    echo "web-entrypoint: 未设 AG_API_UPSTREAM，/api/ 走 SPA fallback（纯静态）" >&2
fi

# 2) envsubst 只替换我们认识的变量（不碰 $uri/$host/...）
export NGINX_ENVSUBST_VAR="${AG_API_UPSTREAM:-}"
envsubst '${AG_API_UPSTREAM}' < "$RUNTIME" > "$OUT"

# 3) 动态解析（可选）：上游是域名时，nginx 默认只在启动时解析一次
if [ -n "${AG_NGINX_RESOLVER:-}" ]; then
    sed -i "s|proxy_pass         ${AG_API_UPSTREAM};|resolver ${AG_NGINX_RESOLVER} valid=30s ipv6=off;\n        set \$upstream_endpoint ${AG_API_UPSTREAM};\n        proxy_pass \$upstream_endpoint;|" "$OUT" || true
fi

# 4) 语法自检：模板渲染错了要在启动前就失败（而不是起来后 502）
if ! nginx -t -c /etc/nginx/nginx.conf; then
    echo "web-entrypoint: nginx -t 失败，渲染结果如下：" >&2
    sed -e 's/^/    /' "$OUT" >&2
    exit 1
fi

exec nginx -g 'daemon off;'
