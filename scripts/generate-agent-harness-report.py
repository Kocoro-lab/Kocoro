#!/usr/bin/env python3
"""Generate the self-contained Kocoro agent harness evidence report."""

from __future__ import annotations

import argparse
import datetime as dt
import html
import json
import pathlib
import subprocess


def read_json(path: pathlib.Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def git(repo: pathlib.Path, *args: str) -> str:
    return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()


def esc(value: object) -> str:
    return html.escape(str(value), quote=True)


def pct(value: float) -> str:
    return f"{value * 100:.1f}%"


def ms(value: float) -> str:
    return f"{value:,.0f} ms"


def usd(value: float) -> str:
    return f"${value:.6f}"


def code_html(value: str) -> str:
    rendered = []
    for line in value.split("\n"):
        trailing_spaces = len(line) - len(line.rstrip(" "))
        core = line[:-trailing_spaces] if trailing_spaces else line
        rendered.append(html.escape(core, quote=True).replace("\t", "&#9;") + "&#32;" * trailing_spaces)
    return "\n".join(rendered)


def code_block(title: str, value: str, open_by_default: bool = False) -> str:
    opened = " open" if open_by_default else ""
    return (
        f"<details{opened}><summary>{esc(title)} <span>{len(value):,} chars</span></summary>"
        f"<pre><code>{code_html(value)}</code></pre></details>"
    )


def prompt_rows(report: dict) -> str:
    rows = []
    for item in report.get("summary", []):
        name = item["name"]
        role = "实测效率对照，不可直接上线" if name == "minimal_v1" else "生产候选" if name.startswith("layered") else "当前生产"
        rows.append(
            "<tr>"
            f"<th>{esc(name)}<small>{role}</small></th>"
            f"<td>{item['successful_tasks']}/{item['runs']}</td>"
            f"<td>{item['correct_tool_runs']}/{item['runs']}</td>"
            f"<td>{ms(item['total_p50_millis'])}</td>"
            f"<td>{ms(item['total_p95_millis'])}</td>"
            f"<td>{item['input_tokens_mean']:,.0f}</td>"
            f"<td>{usd(item['cost_usd_total'])}</td>"
            "</tr>"
        )
    return "".join(rows)


def memory_rows(report: dict) -> str:
    rows = []
    for name in ("model", "preflight"):
        item = report["summary"][name]
        rows.append(
            "<tr>"
            f"<th>{'主模型主动召回' if name == 'model' else '旧 preflight 对照'}</th>"
            f"<td>{item['correct']}/{item['runs']} ({pct(item['accuracy'])})</td>"
            f"<td>{pct(item['positive']['routing_recall'])}</td>"
            f"<td>{pct(item['routing_efficiency_rate'])}</td>"
            f"<td>{pct(item['positive']['answer_success'])}</td>"
            f"<td>{item['negative']['false_memory_queries']}/{item['negative']['runs']}</td>"
            f"<td>{ms(item['mean_latency_ms'])}</td>"
            f"<td>{usd(item['total_cost_usd'])}</td>"
            "</tr>"
        )
    return "".join(rows)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path(__file__).resolve().parents[1])
    parser.add_argument("--prompt-audit", type=pathlib.Path, required=True)
    parser.add_argument("--prompt-report", type=pathlib.Path, required=True)
    parser.add_argument("--memory-report", type=pathlib.Path, required=True)
    parser.add_argument("--loop-report", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()

    repo = args.repo.resolve()
    prompt_audit = read_json(args.prompt_audit)
    prompt_report = read_json(args.prompt_report)
    memory_report = read_json(args.memory_report)
    loop_report = read_json(args.loop_report)
    output_rel = args.output.resolve().relative_to(repo)
    committed_diff = git(
        repo,
        "diff",
        "--no-ext-diff",
        "origin/main...HEAD",
        "--",
        ".",
        f":(exclude){output_rel}",
    )
    working_diff = git(repo, "diff", "--no-ext-diff", "--", ".", f":(exclude){output_rel}")
    full_diff = committed_diff + ("\n\n" + working_diff if working_diff else "")
    layers = prompt_audit["layers"]
    full = prompt_audit["koe_full"]
    fast = prompt_audit["koe_fast"]
    current = next(x for x in prompt_report["summary"] if x["name"] == "current")
    minimal = next(x for x in prompt_report["summary"] if x["name"] == "minimal_v1")
    conditional = next(x for x in prompt_report["summary"] if x["name"] == "layered_conditional_v1")
    memory_failures = [x for x in memory_report["results"] if not x["correct"]]
    model_memory = memory_report["summary"]["model"]
    preflight_memory = memory_report["summary"]["preflight"]
    memory_cost_reduction = (1 - model_memory["total_cost_usd"] / preflight_memory["total_cost_usd"]) * 100
    memory_mean_delta = (model_memory["mean_latency_ms"] / preflight_memory["mean_latency_ms"] - 1) * 100
    memory_median_reduction = (1 - model_memory["median_latency_ms"] / preflight_memory["median_latency_ms"]) * 100
    now = dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")

    document = f"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Kocoro Agent Harness Review</title>
<style>
:root{{--paper:#f5f7fb;--panel:#fff;--ink:#14213d;--muted:#5c6882;--line:#d8deea;--blue:#2457ff;--cyan:#00a7c7;--mint:#168c68;--amber:#b76b00;--red:#c43d4b;--code:#10182b}}
*{{box-sizing:border-box}}html{{scroll-behavior:smooth}}body{{margin:0;background:linear-gradient(90deg,#edf1f8 1px,transparent 1px),linear-gradient(#edf1f8 1px,transparent 1px),var(--paper);background-size:24px 24px;color:var(--ink);font:15px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}}
a{{color:var(--blue)}}.shell{{display:grid;grid-template-columns:230px minmax(0,1fr);max-width:1500px;margin:auto}}nav{{position:sticky;top:0;height:100vh;padding:32px 22px;border-right:1px solid var(--line);background:rgba(245,247,251,.93);backdrop-filter:blur(14px)}}nav strong{{display:block;font:800 22px/1.05 ui-rounded,"Arial Rounded MT Bold",sans-serif;letter-spacing:-.04em;margin-bottom:28px}}nav a{{display:block;padding:7px 0;color:var(--muted);text-decoration:none}}nav a:hover,nav a:focus{{color:var(--blue)}}main{{min-width:0;padding:52px clamp(24px,5vw,76px) 100px}}header{{max-width:1040px;padding:0 0 42px;border-bottom:3px solid var(--ink)}}.eyebrow{{font:700 12px/1 ui-monospace,SFMono-Regular,monospace;letter-spacing:.13em;color:var(--blue);text-transform:uppercase}}h1{{max-width:900px;margin:17px 0 16px;font:800 clamp(42px,7vw,86px)/.94 ui-rounded,"Arial Rounded MT Bold",sans-serif;letter-spacing:-.065em}}.lede{{font-size:20px;max-width:850px;color:#37435c}}.verdict{{display:grid;grid-template-columns:16px 1fr;gap:18px;margin:34px 0 0;padding:22px;background:#eef2ff;border:1px solid #b9c7ff;border-radius:14px}}.verdict i{{display:block;background:var(--blue);border-radius:20px}}section{{padding:55px 0;border-bottom:1px solid var(--line)}}h2{{margin:0 0 10px;font:800 34px/1.05 ui-rounded,"Arial Rounded MT Bold",sans-serif;letter-spacing:-.035em}}h3{{margin:28px 0 10px;font-size:19px}}.sub{{max-width:880px;color:var(--muted);margin:0 0 26px}}.grid{{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:14px}}.card{{position:relative;padding:20px;background:var(--panel);border:1px solid var(--line);border-radius:14px;box-shadow:0 8px 28px rgba(20,33,61,.05)}}.card:before{{content:"";position:absolute;left:20px;right:20px;top:0;height:3px;background:var(--blue)}}.card strong{{display:block;font-size:24px;line-height:1.1;margin-bottom:8px}}.card small,th small{{display:block;color:var(--muted);font-weight:500}}.ok{{color:var(--mint)}}.warn{{color:var(--amber)}}.bad{{color:var(--red)}}table{{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line);font-variant-numeric:tabular-nums}}th,td{{padding:12px 13px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}}thead th{{background:#e9edf5;font:700 12px/1.2 ui-monospace,SFMono-Regular,monospace;letter-spacing:.04em}}tbody th{{white-space:nowrap}}details{{margin:10px 0;background:var(--panel);border:1px solid var(--line);border-radius:10px;overflow:hidden}}summary{{cursor:pointer;padding:13px 16px;font-weight:700}}summary span{{float:right;color:var(--muted);font:500 12px ui-monospace,monospace}}pre{{margin:0;padding:18px;max-height:72vh;overflow:auto;background:var(--code);color:#dce6ff;font:12px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;word-break:break-word}}.trace{{display:grid;grid-template-columns:120px 22px 1fr;gap:0 14px;margin:24px 0}}.trace .rail{{position:relative}}.trace .rail:before{{content:"";position:absolute;left:9px;top:0;bottom:-34px;width:2px;background:var(--line)}}.trace .rail:after{{content:"";position:absolute;left:4px;top:6px;width:10px;height:10px;border-radius:50%;background:var(--blue);box-shadow:0 0 0 4px #dfe6ff}}.trace time{{color:var(--muted);font:12px ui-monospace,monospace;padding-top:2px}}.trace p{{margin:0 0 28px}}.tag{{display:inline-block;padding:2px 7px;border-radius:99px;background:#e7ecf6;color:var(--muted);font:700 11px ui-monospace,monospace}}.callout{{padding:18px 20px;border-left:4px solid var(--amber);background:#fff8e8}}footer{{padding-top:35px;color:var(--muted);font:12px ui-monospace,monospace}}@media(max-width:900px){{.shell{{display:block}}nav{{position:relative;height:auto;border-right:0;border-bottom:1px solid var(--line)}}nav a{{display:inline-block;margin-right:14px}}main{{padding-top:36px}}.grid{{grid-template-columns:1fr}}.trace{{grid-template-columns:82px 18px 1fr}}table{{display:block;overflow:auto}}}}@media(prefers-reduced-motion:reduce){{html{{scroll-behavior:auto}}}}
</style>
</head>
<body><div class="shell">
<nav><strong>Kocoro<br>Harness Review</strong><a href="#verdict">结论</a><a href="#runtime">运行合理性</a><a href="#memory">Memory A/B</a><a href="#prompt">Prompt A/B</a><a href="#layers">完整提示词</a><a href="#comparison">横向对比</a><a href="#gaps">剩余缺口</a><a href="#diff">完整 Diff</a></nav>
<main>
<header id="verdict"><div class="eyebrow">worktree evidence / experiment/kocoro-agent-lab / {esc(now)}</div><h1>运行骨架是强的，提示词还没有完成瘦身。</h1><p class="lede">工具轨迹、停止条件、并发、幂等和循环防护已经达到很高水准；但真实 sidecar memory、自然工作质量和完整工具生态仍缺 release 级证据，因此现在不能称为“顶级 harness 已完成”。</p><div class="verdict"><i></i><div><strong>本轮产品决策</strong><br>生产路径移除 memory preflight，由主模型按需调用 <code>memory_recall</code>；保留旧 preflight 仅作实验对照。最终 3× paired A/B 两条路径均 24/24，主模型路径成本低 {memory_cost_reduction:.1f}%、中位延迟快 {memory_median_reduction:.1f}%，但平均延迟因尾部 outlier 慢 {memory_mean_delta:.1f}%，不能宣传为全面提速。</div></div></header>

<section id="runtime"><div class="eyebrow">01 / trajectory, not pass count</div><h2>整体运行合理性</h2><p class="sub">判断单位是完整轨迹：模型调用次数 = 工具轮数 + 最终回答；精确工具名和参数；串行依赖、并行同轮；空结果后停止；副作用不重复；结果变化时不能误杀。</p><div class="grid"><div class="card"><strong class="ok">132 / 132</strong>live prompt runs 工具轨迹正确<small>11 workloads × 4 variants × 3 reps</small></div><div class="card"><strong class="ok">0</strong>重复工具与重复副作用<small>无 retry；completion 拓扑合理</small></div><div class="card"><strong class="ok">0 / 10</strong>loop corpus 误杀或漏检<small>5 productive + 5 genuine loops</small></div></div>
<div class="trace"><time>productive</time><span class="rail"></span><p>同参数 polling 只要观察结果持续变化，允许到达完成；多来源研究、浏览器多步、不同 HTTP 批次均未触发。</p><time>warning</time><span class="rail"></span><p>无进展的 identical polling 在第 3 次提醒；ping-pong 在第 6 次提醒，给模型一次修正机会。</p><time>force stop</time><span class="rail"></span><p>持续无进展才强停。危险 read loop 会在执行前阻断整个批次，避免“部分批次已经产生副作用”。</p><time>recovery</time><span class="rail"></span><p>写工具的 post-dispatch transport error 不重放；只有明确 replay-safe 的工具才恢复。daemon idempotency 可重放交付回执，不重复 LLM 或副作用。</p></div>
{code_block('离线 loop detector 完整结果', json.dumps(loop_report, ensure_ascii=False, indent=2))}</section>

<section id="memory"><div class="eyebrow">02 / model-driven recall</div><h2>Memory：去掉 preflight 的收益主要是成本</h2><p class="sub">最终 2026-08-07 live 配对测试，3 repetitions、48 total runs。fake session_search 的 <code>Info()</code> 直接复用生产 <code>SessionSearchTool.Info()</code>，因此模型看到的工具名称、description 与参数 schema 和真实运行一致；只有 <code>Run()</code> 使用确定性 fixture。synthetic Ready service 只测路由、调用、答案忠实度、延迟与成本；不代表真实用户 bundle、sidecar 检索或排序质量。</p><table><thead><tr><th>路径</th><th>总正确</th><th>正例路由</th><th>路由效率</th><th>正例答案</th><th>负例误召回</th><th>平均延迟</th><th>总成本</th></tr></thead><tbody>{memory_rows(memory_report)}</tbody></table>
<div class="callout"><strong>最终结果：</strong>失败 {len(memory_failures)} 条。主模型路径平均 {ms(model_memory['mean_latency_ms'])}，旧 preflight {ms(preflight_memory['mean_latency_ms'])}；正例主模型更慢，但负例更快，provider 尾延迟仍显著。调试中的旧 harness 曾漏注册 session_search、误把 honorific 当必需、把多余复查算成功；这些 evaluator 缺陷已修复后才得到本表。</div>
{code_block('最新 Memory A/B 原始 JSON', json.dumps(memory_report, ensure_ascii=False, indent=2))}</section>

<section id="prompt"><div class="eyebrow">03 / controlled prompt experiment</div><h2>提示词：当前正确，但明显过重</h2><p class="sub">这不是 Fast/Full 对决，而是固定同一 AgentLoop、工具、模型和 workload，仅替换 system instructions。reported winner 只在允许上线的候选中选择；minimal 是压力对照，不能被选择器自动上线。</p><table><thead><tr><th>Variant</th><th>任务</th><th>工具</th><th>P50</th><th>P95</th><th>均值 input tokens</th><th>成本</th></tr></thead><tbody>{prompt_rows(prompt_report)}</tbody></table>
<div class="grid" style="margin-top:14px"><div class="card"><strong>{len(full['system']):,} chars</strong>代表性 Koe system prompt<small>实际每个 registry / agent 会变化</small></div><div class="card"><strong class="ok">minimal_v1</strong>实测效率最佳<small>P95 {ms(minimal['total_p95_millis'])}，但覆盖不足，不可上线</small></div><div class="card"><strong class="warn">layered_conditional_v1</strong>合格生产候选<small>tokens 比 current 少 {(1-conditional['input_tokens_mean']/current['input_tokens_mean'])*100:.0f}%，但首语义更慢</small></div></div>
<p><strong>结论：</strong>提示词优化还没好。当前 production 在 33/33 任务与工具轨迹上稳定，但 26k 级 system instructions 是明显负担；不能直接换 minimal，也不能因为 selector 报 conditional winner 就宣称全局最佳。</p>
{code_block('Prompt live comparison 原始 JSON', json.dumps(prompt_report, ensure_ascii=False, indent=2))}</section>

<section id="layers"><div class="eyebrow">04 / exact assembled text</div><h2>Kocoro / Koe Full / Koe Fast 完整提示词</h2><p class="sub">Koe 不是另一套人格。Kocoro base prompt 进入共享 system；Koe Full 与 Fast 使用相同 system/stable context，Fast 仅在 volatile context 增加 outcome-first 规则。下面由 production builder 实际导出，非手工摘要。</p>
<h3>Kocoro 层</h3>{code_block('defaultPersona（含 persona_note）', layers['default_persona'], True)}{code_block('coreOperationalRules', layers['core_operational_rules'])}{code_block('contrastExamplesCore', layers['core_contrast_examples'])}{code_block('Kocoro base prompt（上述三层完整拼接）', layers['kocoro_base_prompt'])}
<h3>Koe Full</h3>{code_block('Full / system', full['system'])}{code_block('Full / stable_context', full['stable_context'])}{code_block('Full / volatile_context', full['volatile_context'], True)}
<h3>Koe Fast</h3>{code_block('Fast / system（与 Full 字节相同）', fast['system'])}{code_block('Fast / stable_context', fast['stable_context'])}{code_block('Fast / volatile_context（含 Fast Task）', fast['volatile_context'], True)}
<h3>条件层</h3>{code_block('cloud_delegation_guidance', layers['cloud_delegation_guidance'])}{code_block('cloud contrast examples', layers['cloud_contrast_examples'])}</section>

<section id="comparison"><div class="eyebrow">05 / local source snapshots</div><h2>Codex / Hermes / OpenClaw 对比</h2><p class="sub">基于本地 study checkout：Codex 7a0e974e08、Hermes 8f2712725、OpenClaw 0ce5e358d4b。比较的是设计方法，不把 coding-agent 专用规则原样搬进日常助手。</p><table><thead><tr><th>系统</th><th>Prompt 组织</th><th>工具/循环</th><th>Memory</th><th>Kocoro 应吸收什么</th></tr></thead><tbody>
<tr><th>Codex</th><td>model base instructions + developer/user layers；运行边界由 sandbox/approval/goal 状态承载</td><td>强 runtime contract 与恢复证据，不主要依赖“多写几句提示词”</td><td>线程状态、生成/污染模式分离</td><td>把不变量下沉到 runtime；提示词只保留模型必须判断的语义</td></tr>
<tr><th>Hermes</th><td>stable/workspace/trailing 分层，cache-aware builder；personality、skills、memory 可插拔</td><td>较高 max turns，依赖分层上下文和专门子流程</td><td>provider 插件 + prompt block，也有 query rewrite helper</td><td>保留 Kocoro 的 stable/volatile 缓存设计；不要默认增加 helper</td></tr>
<tr><th>OpenClaw</th><td>大型 section builder + cache boundary + channel/plugin additions</td><td>generic repeat、known-poll no-progress、ping-pong detector；完整 loop detection 默认可配置</td><td>memory_search + exclusive memory plugin slot</td><td>Kocoro 当前 outcome-aware detector 更积极；应继续补真实工具生态回放</td></tr>
<tr><th>Kocoro</th><td>persona + core rules + examples + static/stable/volatile</td><td>9 类 detector、批次前阻断、幂等/unknown-outcome 合约</td><td>sidecar + session/MEMORY fallback；现改为主模型主动 recall</td><td>运行骨架领先项保留，26k prompt 继续按证据减重</td></tr></tbody></table></section>

<section id="gaps"><div class="eyebrow">06 / still open</div><h2>当前仍未关闭的缺口</h2><table><thead><tr><th>优先</th><th>缺口</th><th>为什么重要</th><th>完成标准</th></tr></thead><tbody>
<tr><th><span class="tag">NOW</span></th><td>真实 sidecar / 用户 bundle memory E2E</td><td>现有 A/B 是 deterministic service，不能证明 nightly replay、实体解析和真实召回率</td><td>隔离 bundle，至少 3 repetitions；路由、query shape、with-data、答案忠实度分别统计</td></tr>
<tr><th><span class="tag">NOW</span></th><td>自然日常工作质量</td><td>receipt workload 擅长查“是否照流程跑”，不测写信、总结、规划、语音短答是否好用</td><td>盲评 rubric + 可复现 fixtures，质量与速度分开</td></tr>
<tr><th><span class="tag">NEXT</span></th><td>真实 MCP / browser / computer / apps registry</td><td>当前 prompt A/B 使用 in-memory tools，无法覆盖 auth、UI state、tool_search 大 catalog</td><td>本地 dev daemon 的 read-only 和可回滚写入 seam</td></tr>
<tr><th><span class="tag">NEXT</span></th><td>长轨迹与崩溃恢复</td><td>3-call serial 不代表 10+ steps、compaction、外部写后进程崩溃</td><td>checkpoint + receipt + restart，证明不重复副作用</td></tr>
<tr><th><span class="tag">NEXT</span></th><td>跨 profile / provider 与稀有失败率</td><td>3 reps 只能用于比较，不能估算 1% 以下失败</td><td>Fast/Full 各自固定模型条件，扩大样本并报告置信区间</td></tr></tbody></table></section>

<section id="diff"><div class="eyebrow">07 / source of truth</div><h2>完整修改 Diff</h2><p class="sub">范围为 <code>origin/main...HEAD</code> 的完整分支差异，加上生成本报告前的未提交差异；HTML 自身被排除，避免递归嵌套。</p>{code_block('完整 diff', full_diff)}</section>
<footer>Generated by scripts/generate-agent-harness-report.py · evidence files remain machine-readable · no external assets</footer>
</main></div></body></html>"""
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(document, encoding="utf-8")


if __name__ == "__main__":
    main()
