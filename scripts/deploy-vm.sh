#!/bin/bash
# VM 环境标准化部署脚本
# 用法: ./deploy-vm.sh [full|quick]
#   full: 完整部署（停止 + 删除数据库 + 上传二进制 + 启动）
#   quick: 快速部署（停止 + 上传二进制 + 启动，保留数据库）

set -e

VM_HOST="10.21.20.65"
VM_PORT="39022"
VM_USER="Docker"
VM_DIR="C:\\Users\\Docker\\Desktop\\Workspace\\phaethon"
VM_DB="$VM_DIR\\phaethon.db"

SSH_CMD="ssh -p $VM_PORT $VM_USER@$VM_HOST"
SCP_CMD="scp -P $VM_PORT"

MODE=${1:-full}

echo "=== VM 部署脚本 (模式: $MODE) ==="

# 1. 编译
echo "[1/5] 编译 Windows 二进制..."
make windows

# 2. 上传到临时位置
echo "[2/5] 上传到 VM 临时位置..."
$SCP_CMD dist/windows-amd64/phaethon.exe $VM_USER@$VM_HOST:/c/temp/phaethon.exe

# 3. 验证文件哈希
echo "[3/5] 验证文件完整性..."
LOCAL_HASH=$(md5 dist/windows-amd64/phaethon.exe | awk '{print $4}' | tr '[:upper:]' '[:lower:]')
REMOTE_HASH=$($SSH_CMD "powershell -Command '(Get-FileHash C:\\temp\\phaethon.exe -Algorithm MD5).Hash.ToLower()'" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')
if [ "$LOCAL_HASH" != "$REMOTE_HASH" ]; then
    echo "错误: 文件哈希不匹配!"
    echo "  本地: $LOCAL_HASH"
    echo "  远程: $REMOTE_HASH"
    exit 1
fi
echo "  ✓ 哈希匹配: $LOCAL_HASH"

# 4. 停止 + (可选)删除数据库 + 替换二进制 + 启动
echo "[4/5] 部署服务..."
if [ "$MODE" = "full" ]; then
    echo "  完整模式: 删除数据库，重新初始化..."
    $SSH_CMD "powershell -Command \"Stop-ScheduledTask -TaskName PhaethonTUN -ErrorAction SilentlyContinue; Stop-Process -Name phaethon -Force -ErrorAction SilentlyContinue; Start-Sleep -Seconds 2; Remove-Item -Force '$VM_DB' -ErrorAction SilentlyContinue; Remove-Item -Force '$VM_DIR\\phaethon.exe' -ErrorAction SilentlyContinue; Copy-Item -Force 'C:\\temp\\phaethon.exe' '$VM_DIR\\phaethon.exe'; Start-ScheduledTask -TaskName PhaethonTUN\""
else
    echo "  快速模式: 保留数据库..."
    $SSH_CMD "powershell -Command \"Stop-ScheduledTask -TaskName PhaethonTUN -ErrorAction SilentlyContinue; Stop-Process -Name phaethon -Force -ErrorAction SilentlyContinue; Start-Sleep -Seconds 2; Remove-Item -Force '$VM_DIR\\phaethon.exe' -ErrorAction SilentlyContinue; Copy-Item -Force 'C:\\temp\\phaethon.exe' '$VM_DIR\\phaethon.exe'; Start-ScheduledTask -TaskName PhaethonTUN\""
fi

# 5. 验证部署
echo "[5/5] 验证部署..."
sleep 5

# 检查进程
PROCESS_COUNT=$($SSH_CMD "powershell -Command '(Get-Process phaethon -ErrorAction SilentlyContinue).Count'" 2>/dev/null || echo "0")
if [ "$PROCESS_COUNT" -lt 2 ]; then
    echo "错误: 进程未启动或只有 $PROCESS_COUNT 个进程（应该有 2 个：watchdog + worker）"
    $SSH_CMD "powershell -Command 'Get-Content $VM_DIR\\data\\logs\\phaethon.log -Tail 20'"
    exit 1
fi
echo "  ✓ 进程运行中: $PROCESS_COUNT 个"

# 检查端口
PORT_LISTENING=$($SSH_CMD "powershell -Command 'if (netstat -ano | findstr :39999 | findstr LISTENING) { \"yes\" } else { \"no\" }'" 2>/dev/null || echo "no")
if [ "$PORT_LISTENING" != "yes" ]; then
    echo "错误: 端口 39999 未监听"
    exit 1
fi
echo "  ✓ 端口 39999 监听中"

# 检查 API（如果是完整模式，应该无密码）
if [ "$MODE" = "full" ]; then
    echo "  测试 API 访问（无密码）..."
    HTTP_CODE=$(curl -s -k -o /dev/null -w "%{http_code}" --connect-timeout 5 https://$VM_HOST:39999/api/health 2>/dev/null || echo "000")
    if [ "$HTTP_CODE" = "401" ]; then
        echo "警告: API 返回 401，可能需要密码（数据库初始化可能未使用 InitDefault）"
        echo "  请检查日志确认初始化流程"
    elif [ "$HTTP_CODE" = "200" ]; then
        echo "  ✓ API 可访问（无密码）"
    else
        echo "警告: API 返回 HTTP $HTTP_CODE"
    fi
fi

echo ""
echo "=== 部署完成 ==="
echo "访问地址: https://$VM_HOST:39999/"
if [ "$MODE" = "full" ]; then
    echo "数据库: 已重新初始化（无密码）"
    echo "下一步: 修改 nodeId 为 'vm'"
fi
