#!/usr/bin/env python3
"""把 Qoder 网关支持的模型追加到 Codex 的 bridge_models.json。

用法:
    python tools/add_qoder_models.py --dry-run
    python tools/add_qoder_models.py --prefix ""        # 裸 slug（默认，ss provider）
    python tools/add_qoder_models.py --prefix "qoder:"  # 带 provider 前缀

来源: GET http://127.0.0.1:8790/v1/models

设计要点（每一条都有对应的坑）:
  * **幂等**：按 slug 去重，重复运行不会产生重复条目。
  * **只增不改**：已有的 slug（含其他 provider 的模型）一律跳过，绝不覆盖。
  * **先备份**：写入前按 bridge_models.json.bak-<时间戳> 备份，与目录里既有的
    备份命名习惯一致。
  * **原子写**：先写临时文件再 os.replace，崩溃不会留下半个 JSON。
  * **沿用既有条目形状**：字段集合与顺序对齐现有 77 个条目，避免 Codex 读到
    半认识的条目。
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
import time
import urllib.request

CATALOG = os.path.expanduser(r"~/.codex/bridge_models.json")

# Codex 只认这四档（现有 77 条目的实测取值）。
EFFORT_META = {
    "low": ("Fast", 1),
    "medium": ("Balanced", 2),
    "high": ("Deep", 4),
    "xhigh": ("Deepest", 5),
}

# Qoder 的档位 -> Codex 档位。max 没有对应档，压到 xhigh。
EFFORT_MAP = {"minimal": "low", "low": "low", "medium": "medium",
              "high": "high", "xhigh": "xhigh", "max": "xhigh", "none": None}

BASE_INSTRUCTIONS = (
    "You are Codex, an agent based on GPT-5. You share one workspace with the "
    "user and collaborate until their goal is genuinely handled.\n\n"
    "# Rules for getting work done\n"
    "- Prefer `rg` for searching; parallelize independent tool calls.\n"
    "- Keep changes surgical; prefer the simplest approach; match existing style.\n"
    "- Read adjacent code before writing.\n"
    "- Route retries, arithmetic, and time through deterministic code, not prompts.\n"
    "- Give every loop a ceiling; avoid long blocking waits.\n"
    "- Never use destructive commands (`git reset --hard`, `git checkout --`) "
    "unless asked.\n\n"
    "# File editing\n"
    "Use `apply_patch` for local edits; do not create/edit files with shell write "
    "tricks. Preserve unrelated user changes in a dirty worktree.\n\n"
    "# Destructive actions\n"
    "Be cautious with deleting/overwriting commands; confirm targets with read-only "
    "checks; prefer recoverable operations; never point recursive deletion at a "
    "broad root. After deleting anything material, say what was removed.\n"
)


def fetch_models(base: str) -> list[dict]:
    url = base.rstrip("/") + "/v1/models"
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=20) as resp:
        if resp.status != 200:
            raise SystemExit(f"{url} -> HTTP {resp.status}")
        return json.load(resp)["data"]


def slug_for(model_id: str, prefix: str) -> str:
    """slug 保持官方模型名（网关按 display_name 解析，大小写不敏感）。"""
    return prefix + model_id


def reasoning_levels(efforts: list[str] | None) -> tuple[list[dict], str]:
    """把 Qoder 的档位表换成 Codex 的四档，返回 (levels, default)。"""
    mapped: list[str] = []
    for e in efforts or []:
        c = EFFORT_MAP.get(str(e).lower())
        if c and c not in mapped:
            mapped.append(c)
    if not mapped:
        # 无档位表（如 Auto / Fast）：给三档，Codex 侧仍可调，网关会 clamp。
        mapped = ["low", "medium", "high"]
    mapped.sort(key=lambda x: EFFORT_META[x][1])
    levels = [{"description": EFFORT_META[m][0], "effort": m} for m in mapped]
    default = "medium" if "medium" in mapped else mapped[len(mapped) // 2]
    return levels, default


def build_entry(m: dict, prefix: str, priority: int) -> dict:
    display = m["id"]
    ctx = int(m.get("context_length") or 200000)
    # 官方清单没有 max_output_tokens（网关也不编造）；Codex 需要一个数值，
    # 取上下文的一档保守值，并在 description 里标出来源。
    out_tokens = int(m.get("max_output_tokens") or min(64000, ctx))
    levels, default_level = reasoning_levels(m.get("reasoning_efforts"))
    vision = bool((m.get("capabilities") or {}).get("vision"))
    free = " free" if m.get("is_free") else ""
    windows = m.get("context_window_labels") or []
    return {
        "display_name": display,
        "default_reasoning_level": default_level,
        "supported_reasoning_levels": levels,
        "additional_speed_tiers": [],
        "supports_parallel_tool_calls": True,
        "context_window": ctx,
        "model_messages": {
            "instructions_template": BASE_INSTRUCTIONS,
            "instructions_variables": None,
            "approvals": None,
            "auto_review": None,
            "permissions": None,
        },
        "slug": slug_for(display, prefix),
        "supports_search_tool": False,
        "visibility": "list",
        "use_responses_lite": True,
        "priority": priority,
        "web_search_tool_type": "text_and_image",
        "effective_context_window_percent": 95,
        "supported_in_api": True,
        "supports_image_detail_original": True,
        "base_instructions": BASE_INSTRUCTIONS,
        "include_skills_usage_instructions": False,
        "support_verbosity": True,
        "comp_hash": "3000",
        "experimental_supported_tools": [],
        "availability_nux": None,
        "default_reasoning_summary": "none",
        "max_context_window": ctx,
        "service_tiers": [],
        "description": f"[qoder{'/' + free.strip() if free else ''}] {m.get('upstream_key')}"
                       f" - ctx {ctx} - out {out_tokens}"
                       f"{' - windows ' + '/'.join(windows) if windows else ''}",
        "shell_type": "shell_command",
        "apply_patch_tool_type": "freeform",
        "truncation_policy": {"mode": "tokens", "limit": 10000},
        "upgrade": None,
        "default_verbosity": "low",
        "tool_mode": "code_mode_only",
        "input_modalities": ["text", "image"] if vision else ["text"],
        "multi_agent_version": "v2",
        "supports_reasoning_summaries": False,
        "auto_compact_token_limit": int(ctx * 0.48),
        "max_output_tokens": out_tokens,
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", default="http://127.0.0.1:8790")
    ap.add_argument("--catalog", default=CATALOG)
    ap.add_argument("--prefix", default="",
                    help='slug 前缀；留空表示裸 slug（ss provider）')
    ap.add_argument("--priority", type=int, default=4,
                    help="新条目的优先级（现有 station 模型是 2）")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    models = fetch_models(args.base)
    with open(args.catalog, encoding="utf-8") as f:
        doc = json.load(f)
    existing = doc.setdefault("models", [])
    seen = {m.get("slug") for m in existing}

    added, skipped = [], []
    for m in models:
        if not m.get("enabled", True):
            skipped.append((m["id"], "disabled"))
            continue
        entry = build_entry(m, args.prefix, args.priority)
        if entry["slug"] in seen:
            skipped.append((m["id"], "already present"))
            continue
        added.append(entry)
        seen.add(entry["slug"])

    print(f"gateway models : {len(models)}")
    print(f"already present: {len(skipped) - sum(1 for _, r in skipped if r == 'disabled')}")
    print(f"disabled       : {sum(1 for _, r in skipped if r == 'disabled')}")
    print(f"to add         : {len(added)}")
    for e in added:
        print(f"  + {e['slug']:<28} ctx={e['context_window']:<8} "
              f"out={e['max_output_tokens']:<6} default={e['default_reasoning_level']:<7} "
              f"mods={','.join(e['input_modalities'])}")

    if args.dry_run or not added:
        if not added:
            print("\nnothing to do")
        return 0

    existing.extend(added)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    backup = f"{args.catalog}.bak-{stamp}"
    shutil.copy2(args.catalog, backup)
    tmp = args.catalog + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, indent=1)
        f.write("\n")
    os.replace(tmp, args.catalog)
    print(f"\nwrote {args.catalog} ({len(existing)} models total)")
    print(f"backup {backup}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
