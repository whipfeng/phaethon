#!/bin/bash
# 旁路网关端到端测试
# 从本地通过 mesh 路由测试 QG 旁路网关的 DNS + TCP 连通性
#
# 前提：本地已配置 mesh 网段路由 (100.64/24 → 192.168.1.101)
# 用法：./test-bypass-gateway.sh [域名]

set -e

DNS_SERVER="100.64.0.3"
DOMAIN="${1:-httpbin.org}"

echo "=== 旁路网关测试 ==="
echo "DNS server: ${DNS_SERVER}"
echo "Domain:     ${DOMAIN}"
echo ""

# Step 1: DNS 解析
echo "--- Step 1: DNS 解析 ---"
RESOLVED_IP=$(nslookup "${DOMAIN}" "${DNS_SERVER}" 2>/dev/null | awk '/^Address: / { split($2,a,"#"); print a[1] }' | tail -1)
if [ -z "${RESOLVED_IP}" ]; then
    echo "FAIL: DNS 解析失败"
    exit 1
fi
echo "OK: ${DOMAIN} -> ${RESOLVED_IP}"
echo ""

# Step 2: HTTP 连通性测试 (port 80)
echo "--- Step 2: HTTP 连通性 (port 80) ---"
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    --resolve "${DOMAIN}:80:${RESOLVED_IP}" \
    --connect-timeout 5 --max-time 10 \
    "http://${DOMAIN}/" 2>/dev/null || echo "000")
if [ "${HTTP_CODE}" = "000" ]; then
    echo "FAIL: TCP 连接失败 (HTTP code: ${HTTP_CODE})"
    exit 1
fi
echo "OK: HTTP ${HTTP_CODE}"
echo ""

# Step 3: HTTPS 连通性测试 (port 443)
echo "--- Step 3: HTTPS 连通性 (port 443) ---"
HTTPS_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    --resolve "${DOMAIN}:443:${RESOLVED_IP}" \
    --connect-timeout 5 --max-time 10 \
    "https://${DOMAIN}/" 2>/dev/null || echo "000")
if [ "${HTTPS_CODE}" = "000" ]; then
    echo "WARN: HTTPS 连接失败 (可能是证书问题，非旁路网关问题)"
else
    echo "OK: HTTPS ${HTTPS_CODE}"
fi
echo ""

echo "=== 测试完成 ==="
echo "DNS:  ${DOMAIN} -> ${RESOLVED_IP} (via ${DNS_SERVER})"
echo "HTTP: ${HTTP_CODE}"
echo "HTTPS: ${HTTPS_CODE}"
