#!/usr/bin/env python3
"""TraeCLI hook: 上下文用量守卫。

在 user_prompt_submit / pre_compact 时读取当前会话事件日志里最近一次模型请求的
prompt_tokens，按 TRAE_CTX_LIMIT 计算占比；达到 85% 或触发压缩前，用 systemMessage
提示用户手动压缩 / 开新会话。

设计约束：
- 永不阻断（任何异常都静默退出 0）；
- 只读会话日志尾部，避免读取超大 events.jsonl 全量；
- 同一会话做节流：占比增长不足 2% 且非 pre_compact 时不重复提示。

环境变量：
  TRAE_CTX_LIMIT   上下文上限（token），默认 200000
  TRAE_CTX_WARN    告警阈值（0-1），默认 0.85
"""

import glob
import json
import os
import re
import sys
import tempfile
import time

DEFAULT_LIMIT = 200_000
DEFAULT_WARN = 0.85
TAIL_BYTES = 4 * 1024 * 1024


def load_settings():
    """读取限值与阈值：环境变量 > .trae/hooks/ctx_guard.json > 默认值。"""
    limit, warn_at = DEFAULT_LIMIT, DEFAULT_WARN
    cfg = os.path.join(os.path.dirname(os.path.abspath(__file__)), "ctx_guard.json")
    try:
        with open(cfg, "r", encoding="utf-8") as f:
            data = json.load(f)
        limit = int(data.get("limit", limit))
        warn_at = float(data.get("warn", warn_at))
    except Exception:
        pass
    try:
        if os.environ.get("TRAE_CTX_LIMIT"):
            limit = int(os.environ["TRAE_CTX_LIMIT"])
        if os.environ.get("TRAE_CTX_WARN"):
            warn_at = float(os.environ["TRAE_CTX_WARN"])
    except Exception:
        pass
    return limit, warn_at


def read_stdin_json():
    try:
        raw = sys.stdin.buffer.read()
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8", "replace"))
    except Exception:
        return {}


def find_events_file(info):
    # 1) transcript_path 由 CLI 直接给出
    tp = info.get("transcript_path") or ""
    if tp and os.path.isfile(tp) and tp.endswith(".jsonl"):
        return tp
    # 2) 会话目录：%LOCALAPPDATA%\trae-cli\sessions\<session_id>\events.jsonl
    sid = info.get("session_id") or ""
    if sid:
        base = os.environ.get("LOCALAPPDATA") or os.path.expanduser("~/.local/share")
        p = os.path.join(base, "trae-cli", "sessions", sid, "events.jsonl")
        if os.path.isfile(p):
            return p
        hits = glob.glob(os.path.join(base, "trae-cli", "sessions", sid, "*.jsonl"))
        if hits:
            return max(hits, key=os.path.getsize)
    return None


def last_prompt_tokens(path):
    """从文件尾部找最近一次 usage.prompt_tokens。"""
    try:
        size = os.path.getsize(path)
        with open(path, "rb") as f:
            if size > TAIL_BYTES:
                f.seek(size - TAIL_BYTES)
            chunk = f.read()
    except Exception:
        return None
    matches = re.findall(rb'"prompt_tokens"\s*:\s*(\d+)', chunk)
    if not matches:
        return None
    try:
        return int(matches[-1])
    except Exception:
        return None


def state_path(sid):
    safe = re.sub(r"[^A-Za-z0-9_.-]", "_", sid or "unknown")
    return os.path.join(tempfile.gettempdir(), "trae_ctx_guard_%s.json" % safe)


def load_state(sid):
    try:
        with open(state_path(sid), "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {}


def save_state(sid, state):
    try:
        with open(state_path(sid), "w", encoding="utf-8") as f:
            json.dump(state, f)
    except Exception:
        pass


def emit(message):
    sys.stdout.buffer.write(
        json.dumps({"systemMessage": message}, ensure_ascii=False).encode("utf-8")
    )
    sys.stdout.buffer.flush()


def main():
    info = read_stdin_json()
    # CLI 可能给出 PascalCase（PreCompact）或 snake_case（pre_compact）
    event = (info.get("hook_event_name") or "").replace("_", "").lower()
    sid = info.get("session_id") or ""
    is_pre_compact = event == "precompact"

    limit, warn_at = load_settings()
    if limit <= 0:
        return

    path = find_events_file(info)
    used = last_prompt_tokens(path) if path else None
    pct = (used / limit) if used else None

    if is_pre_compact:
        detail = ""
        if pct is not None:
            detail = "（上次请求 %s / %s tokens，约 %.0f%%）" % (
                format(used, ","),
                format(limit, ","),
                pct * 100,
            )
        emit(
            "⚠ 上下文即将自动压缩%s。若本会话已接近上限，建议现在手动 /compact 或开新会话，"
            "避免压缩请求本身超限而卡死。" % detail
        )
        return

    if pct is None or pct < warn_at:
        return

    state = load_state(sid)
    prev = state.get("pct")
    if prev is not None and pct - float(prev) < 0.02:
        return
    if prev is not None and time.time() - float(state.get("ts", 0)) < 120:
        return

    save_state(sid, {"pct": pct, "ts": time.time()})
    emit(
        "⚠ 上下文已用约 %.0f%%（%s / %s tokens）。建议手动执行 /compact 压缩，"
        "或结束本会话并把关键结论落到文档，避免触发无法完成的自动压缩。"
        % (pct * 100, format(used, ","), format(limit, ","))
    )


if __name__ == "__main__":
    try:
        main()
    except Exception:
        pass
    sys.exit(0)
