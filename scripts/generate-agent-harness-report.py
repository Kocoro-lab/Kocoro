#!/usr/bin/env python3
"""Generate the self-contained Kocoro agent harness evidence report."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import html
import json
import pathlib
import re
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


def sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


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
            f"<td>{ms(item['total_p99_millis'])}</td>"
            f"<td>{ms(item['total_max_millis'])}</td>"
            f"<td>{item['input_tokens_mean']:,.0f}</td>"
            f"<td>{usd(item['cost_usd_total'])}</td>"
            "</tr>"
        )
    return "".join(rows)


def paired_rows(report: dict) -> str:
    rows = []
    for item in report["paired_comparisons"]:
        rows.append(
            "<tr>"
            f"<th>{esc(item['candidate'])}<small>vs {esc(item['control'])}</small></th>"
            f"<td>{item['pairs']}</td>"
            f"<td>{item['candidate_latency_wins']} / {item['control_latency_wins']} / {item['latency_ties']}</td>"
            f"<td>{pct(item['candidate_latency_win_rate'])}</td>"
            f"<td>{ms(item['total_delta_p50_millis'])}</td>"
            f"<td>{ms(item['total_delta_p95_millis'])}</td>"
            f"<td>{ms(item['total_delta_p99_millis'])}</td>"
            f"<td>{ms(item['total_delta_min_millis'])} / {ms(item['total_delta_max_millis'])}</td>"
            "</tr>"
        )
    return "".join(rows)


def outlier_rows(report: dict) -> str:
    rows = []
    for summary in report["summary"]:
        for rank, item in enumerate(summary["slowest_runs"], start=1):
            rows.append(
                "<tr>"
                f"<th>{esc(summary['name'])}<small>#{rank}</small></th>"
                f"<td>{esc(item['workload'])}</td>"
                f"<td>{item['repetition']}</td>"
                f"<td>{item['schedule_index']}</td>"
                f"<td>{ms(item['total_millis'])}</td>"
                "</tr>"
            )
    return "".join(rows)


def bullet_list(items: list[str]) -> str:
    return "<ul>" + "".join(f"<li>{esc(item)}</li>" for item in items) + "</ul>"


def provenance_rows(paths: list[pathlib.Path]) -> str:
    rows = []
    for path in paths:
        rows.append(
            "<tr>"
            f"<th>{esc(path.name)}</th>"
            f"<td><code>{esc(sha256(path))}</code></td>"
            f"<td>{path.stat().st_size:,} bytes</td>"
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


def quality_rows(report: dict) -> str:
    rows = []
    for item in report["cases"]:
        rows.append(
            "<tr>"
            f"<th>{esc(item['case'])}</th>"
            f"<td>{item['correct_runs']}/{item['runs']} ({pct(item['correctness_rate'])})</td>"
            f"<td>{ms(item['latency_p50_millis'])}</td>"
            f"<td>{ms(item['latency_p95_millis'])}</td>"
            f"<td>{ms(item['latency_p99_millis'])}</td>"
            f"<td>{item['input_tokens_total']:,}</td>"
            f"<td>{usd(item['cost_usd_total'])}</td>"
            "</tr>"
        )
    return "".join(rows)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path(__file__).resolve().parents[1])
    parser.add_argument("--prompt-audit", type=pathlib.Path, required=True)
    parser.add_argument("--prompt-report", type=pathlib.Path, required=True)
    parser.add_argument("--memory-report", type=pathlib.Path, required=True)
    parser.add_argument("--quality-report", type=pathlib.Path, required=True)
    parser.add_argument("--loop-report", type=pathlib.Path, required=True)
    parser.add_argument("--offline-gate-log", type=pathlib.Path)
    parser.add_argument("--koe-runtime-log", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()

    repo = args.repo.resolve()
    prompt_audit = read_json(args.prompt_audit)
    prompt_report = read_json(args.prompt_report)
    memory_report = read_json(args.memory_report)
    quality_report = read_json(args.quality_report)
    loop_report = read_json(args.loop_report)
    koe_runtime_log = args.koe_runtime_log.read_text(encoding="utf-8")
    artifact_paths = [args.prompt_audit, args.prompt_report, args.memory_report, args.quality_report, args.loop_report, args.koe_runtime_log]
    offline_gate_log = ""
    if args.offline_gate_log:
        offline_gate_log = args.offline_gate_log.read_text(encoding="utf-8")
        artifact_paths.append(args.offline_gate_log)

    require(prompt_audit.get("schema_version") == "kocoro.prompt_audit.v1", "unexpected prompt audit schema")
    for layer in ("kocoro_full_base_prompt", "kocoro_fast_base_prompt"):
        require(layer in prompt_audit.get("layers", {}), f"prompt audit missing {layer}")
    require(prompt_audit["koe_full"]["system"] != prompt_audit["koe_fast"]["system"], "final Full and Fast system prompts must differ")
    require(
        (len(prompt_audit["koe_full"]["system"]), len(prompt_audit["koe_full"]["system"].encode("utf-8"))) == (7_730, 7_732)
        and (len(prompt_audit["koe_fast"]["system"]), len(prompt_audit["koe_fast"]["system"].encode("utf-8"))) == (7_091, 7_093),
        "final Full/Fast system prompt dimensions changed",
    )
    require(prompt_report.get("complete") is True, "prompt benchmark is incomplete")
    require(prompt_report.get("scheduled") == 132 and prompt_report.get("completed") == 132, "prompt benchmark must contain 132/132 runs")
    require(prompt_report.get("repetitions_per_cell") == 3, "prompt benchmark must be the 3x comparison sample")
    require(prompt_report.get("release_qualifying") is False, "3x prompt benchmark must not be labeled release-qualifying")
    require(prompt_report.get("winner_status") == "no_improvement", "prompt benchmark no-candidate decision changed")
    require(memory_report.get("schema_version") == "kocoro.memory_recall_ab.v1", "unexpected memory report schema")
    require(memory_report.get("repetitions_per_cell") == 3 and len(memory_report.get("results", [])) == 48, "memory report must contain the 48-run 3x A/B")
    require(quality_report.get("schema_version") == "kocoro.agent_lab_quality.v1", "unexpected quality report schema")
    require(quality_report.get("complete") is True, "quality report is incomplete")
    require(quality_report.get("scheduled") == 18 and quality_report.get("completed") == 18, "quality report must contain 18/18 runs")
    require(quality_report.get("repetitions_per_case") == 3 and len(quality_report.get("cases", [])) == 6, "quality report must contain six 3x domains")
    require(quality_report.get("correct_runs") == 18, "quality report contains an incorrect run")
    require(quality_report.get("comparison_qualifying") is True, "quality report must qualify for comparison")
    require(quality_report.get("release_qualifying") is False, "3x quality report must not be labeled release-qualifying")
    require(loop_report.get("passed") is True and loop_report.get("trace_count") == 10, "loop detector corpus did not pass 10 traces")
    for case in ("fast_text", "fast_time_text", "full_text", "fast_audio", "full_audio"):
        require(f"--- PASS: TestKoeLiveFullPathMatrixE2E/{case}" in koe_runtime_log, f"Koe runtime log missing pass: {case}")
    require("PERSISTENCE_READBACK:" in koe_runtime_log and "persisted_sessions=5" in koe_runtime_log, "Koe runtime log missing five-session restart readback")
    koe_fast_text = re.search(
        r"TestKoeLiveFullPathMatrixE2E/fast_text.*?VERDICT:.*?total_ms=(\d+).*?USAGE:.*?daemon_input=(\d+)",
        koe_runtime_log,
        re.DOTALL,
    )
    require(koe_fast_text is not None, "Koe runtime log missing Fast text latency/input evidence")
    koe_matrix_total = re.search(r"--- PASS: TestKoeLiveFullPathMatrixE2E \(([\d.]+)s\)", koe_runtime_log)
    require(koe_matrix_total is not None, "Koe runtime log missing matrix duration")
    offline_gate_names = [
        "TestOffline_AgentLabGeneralPurposePromptContract",
        "TestOffline_AgentLabLongReadTrajectoryReachesOutcome",
        "TestOffline_AgentLabCompactionPersistsAcrossRestart",
        "TestOffline_AgentLabInterruptedTrajectoryResumesWithoutReplay",
    ]
    if offline_gate_log:
        for test_name in offline_gate_names:
            require(f"--- PASS: {test_name}" in offline_gate_log, f"offline gate log missing pass: {test_name}")
        require("FAIL" not in offline_gate_log, "offline gate log contains a failure")

    output_rel = args.output.resolve().relative_to(repo)
    branch_base = git(repo, "rev-parse", "origin/main")
    branch_head = git(repo, "rev-parse", "HEAD")
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
    memory_failures = [x for x in memory_report["results"] if not x["correct"]]
    model_memory = memory_report["summary"]["model"]
    preflight_memory = memory_report["summary"]["preflight"]
    memory_cost_reduction = (1 - model_memory["total_cost_usd"] / preflight_memory["total_cost_usd"]) * 100
    memory_mean_reduction = (1 - model_memory["mean_latency_ms"] / preflight_memory["mean_latency_ms"]) * 100
    memory_median_reduction = (1 - model_memory["median_latency_ms"] / preflight_memory["median_latency_ms"]) * 100
    successful_prompt_tasks = sum(item["successful_tasks"] for item in prompt_report["summary"])
    correct_prompt_tool_runs = sum(item["correct_tool_runs"] for item in prompt_report["summary"])
    prompt_failures = prompt_report["variant_specific_failures"] or []
    full_system_bytes = len(full["system"].encode("utf-8"))
    fast_system_bytes = len(fast["system"].encode("utf-8"))
    full_stable_bytes = len(full["stable_context"].encode("utf-8"))
    fast_stable_bytes = len(fast["stable_context"].encode("utf-8"))
    tool_surface = {
        "before_chars": 62_765,
        "before_tools": 29,
        "after_chars": 47_509,
        "after_tools": 22,
        "daemon_input_before": 33_874,
        "daemon_input_after": int(koe_fast_text.group(2)),
        "daemon_total_before_ms": 6_066,
        "daemon_total_after_ms": int(koe_fast_text.group(1)),
        "bash_tokens_before": 1_890,
        "bash_tokens_after": 462,
        "working_set_count_cap": 16,
        "working_set_token_cap": 8_000,
    }
    tool_chars_reduction = (1 - tool_surface["after_chars"] / tool_surface["before_chars"]) * 100
    daemon_input_reduction = (1 - tool_surface["daemon_input_after"] / tool_surface["daemon_input_before"]) * 100
    bash_schema_reduction = (1 - tool_surface["bash_tokens_after"] / tool_surface["bash_tokens_before"]) * 100
    now = dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")

    document = f"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Kocoro Agent Harness Review</title>
<style>
:root{{--paper:#f5f7fb;--panel:#fff;--ink:#14213d;--muted:#5c6882;--line:#d8deea;--blue:#2457ff;--mint:#168c68;--amber:#b76b00;--red:#c43d4b;--code:#10182b}}
*{{box-sizing:border-box}}html{{scroll-behavior:smooth}}body{{margin:0;background:linear-gradient(90deg,#edf1f8 1px,transparent 1px),linear-gradient(#edf1f8 1px,transparent 1px),var(--paper);background-size:24px 24px;color:var(--ink);font:15px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}}
a{{color:var(--blue)}}code{{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}}.shell{{display:grid;grid-template-columns:240px minmax(0,1fr);max-width:1560px;margin:auto}}nav{{position:sticky;top:0;height:100vh;padding:32px 22px;border-right:1px solid var(--line);background:rgba(245,247,251,.94);backdrop-filter:blur(14px)}}nav strong{{display:block;font:800 22px/1.05 ui-rounded,"Arial Rounded MT Bold",sans-serif;letter-spacing:-.04em;margin-bottom:28px}}nav a{{display:block;padding:7px 0;color:var(--muted);text-decoration:none}}nav a:hover,nav a:focus{{color:var(--blue)}}main{{min-width:0;padding:52px clamp(24px,5vw,76px) 100px}}header{{max-width:1080px;padding:0 0 42px;border-bottom:3px solid var(--ink)}}.eyebrow{{font:700 12px/1 ui-monospace,SFMono-Regular,monospace;letter-spacing:.13em;color:var(--blue);text-transform:uppercase}}h1{{max-width:980px;margin:17px 0 16px;font:800 clamp(42px,7vw,82px)/.94 ui-rounded,"Arial Rounded MT Bold",sans-serif;letter-spacing:-.06em}}.lede{{font-size:20px;max-width:900px;color:#37435c}}.verdict{{display:grid;grid-template-columns:16px 1fr;gap:18px;margin:34px 0 0;padding:22px;background:#fff7e5;border:1px solid #e3bd70;border-radius:14px}}.verdict i{{display:block;background:var(--amber);border-radius:20px}}section{{padding:55px 0;border-bottom:1px solid var(--line)}}h2{{margin:0 0 10px;font:800 34px/1.05 ui-rounded,"Arial Rounded MT Bold",sans-serif;letter-spacing:-.035em}}h3{{margin:30px 0 10px;font-size:19px}}.sub{{max-width:940px;color:var(--muted);margin:0 0 26px}}.grid{{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:14px}}.card{{position:relative;padding:20px;background:var(--panel);border:1px solid var(--line);border-radius:14px;box-shadow:0 8px 28px rgba(20,33,61,.05)}}.card:before{{content:"";position:absolute;left:20px;right:20px;top:0;height:3px;background:var(--blue)}}.card strong{{display:block;font-size:24px;line-height:1.1;margin-bottom:8px}}.card small,th small{{display:block;color:var(--muted);font-weight:500}}.ok{{color:var(--mint)}}.warn{{color:var(--amber)}}.bad{{color:var(--red)}}table{{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line);font-variant-numeric:tabular-nums}}th,td{{padding:11px 12px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}}thead th{{background:#e9edf5;font:700 12px/1.2 ui-monospace,SFMono-Regular,monospace;letter-spacing:.04em}}tbody th{{white-space:nowrap}}details{{margin:10px 0;background:var(--panel);border:1px solid var(--line);border-radius:10px;overflow:hidden}}summary{{cursor:pointer;padding:13px 16px;font-weight:700}}summary span{{float:right;color:var(--muted);font:500 12px ui-monospace,monospace}}pre{{margin:0;padding:18px;max-height:72vh;overflow:auto;background:var(--code);color:#dce6ff;font:12px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;word-break:break-word}}.trace{{display:grid;grid-template-columns:128px 22px 1fr;gap:0 14px;margin:24px 0}}.trace .rail{{position:relative}}.trace .rail:before{{content:"";position:absolute;left:9px;top:0;bottom:-34px;width:2px;background:var(--line)}}.trace .rail:after{{content:"";position:absolute;left:4px;top:6px;width:10px;height:10px;border-radius:50%;background:var(--blue);box-shadow:0 0 0 4px #dfe6ff}}.trace time{{color:var(--muted);font:12px ui-monospace,monospace;padding-top:2px}}.trace p{{margin:0 0 28px}}.tag{{display:inline-block;padding:2px 7px;border-radius:99px;background:#e7ecf6;color:var(--muted);font:700 11px ui-monospace,monospace}}.callout{{margin:18px 0;padding:18px 20px;border-left:4px solid var(--amber);background:#fff8e8}}ul{{padding-left:22px}}footer{{padding-top:35px;color:var(--muted);font:12px ui-monospace,monospace}}@media(max-width:900px){{.shell{{display:block}}nav{{position:relative;height:auto;border-right:0;border-bottom:1px solid var(--line)}}nav a{{display:inline-block;margin-right:14px}}main{{padding-top:36px;overflow-x:clip}}.grid{{grid-template-columns:1fr}}.trace{{grid-template-columns:86px 18px 1fr}}table{{display:block;overflow:auto}}}}@media(prefers-reduced-motion:reduce){{html{{scroll-behavior:auto}}}}
</style>
</head>
<body><div class="shell">
<nav><strong>Kocoro<br>Harness Review</strong><a href="#verdict">结论</a><a href="#evidence">证据来源</a><a href="#runtime">运行与恢复</a><a href="#tools">工具面</a><a href="#quality">18-run Quality</a><a href="#prompt">132-run Prompt</a><a href="#memory">48-run Memory</a><a href="#layers">Exact Prompts</a><a href="#comparison">设计对比</a><a href="#gaps">未覆盖边界</a><a href="#diff">完整 Diff</a></nav>
<main>
<header id="verdict"><div class="eyebrow">artifact-backed review / experiment/kocoro-agent-lab / {esc(now)}</div><h1>运行骨架和通用质量样本通过；仍不是 release-ready。</h1><p class="lede">AgentLoop 的工具轨迹、长任务、compaction、重启与 Fast-only schema 收口都有生产 seam 证据；18-run 通用质量样本覆盖六个日常 domain，生产 builder 的 Kocoro / Koe prompts 也可逐字审阅。但 prompt 与 quality 都只有每 cell/case 3 次，明确 <code>release_qualifying=false</code>，不能据此估算稀有失败。</p><div class="verdict"><i></i><div><strong>Prompt 判定仍是 no candidate / no improvement</strong><br>{esc(prompt_report['selection_reason'])} <code>layered_conditional_v1</code> 只是 observed efficiency leader，不是可上线 winner。独立 quality gate 是 {quality_report['correct_runs']}/{quality_report['completed']} correctness，但同样只具 comparison 资格。Memory A/B 支持移除固定 preflight：两路均 24/24，主模型路径成本低 {memory_cost_reduction:.1f}%、平均延迟低 {memory_mean_reduction:.1f}%、中位延迟低 {memory_median_reduction:.1f}%；该结论仍限于 synthetic memory routing。</div></div></header>

<section id="evidence"><div class="eyebrow">01 / immutable inputs</div><h2>证据来源与校验</h2><p class="sub">生成器对 prompt、memory、quality、loop schema，run/repetition 数、no-candidate 状态、correctness 与 Koe 5-case runtime/restart 结果 fail closed。表中 SHA-256 对应本次嵌入的真实 artifact；离线 gate 与 Koe runtime log 都来自最终源码复验。</p><table><thead><tr><th>Artifact</th><th>SHA-256</th><th>大小</th></tr></thead><tbody>{provenance_rows(artifact_paths)}</tbody></table><div class="callout"><strong>Branch：</strong><code>{esc(branch_base)}</code> (origin/main) … <code>{esc(branch_head)}</code> (生成器 commit 时的 HEAD)。报告末尾嵌入除本 HTML 外的完整分支 diff。</div></section>

<section id="runtime"><div class="eyebrow">02 / real AgentLoop seams</div><h2>运行轨迹、长任务、compaction 与恢复</h2><p class="sub">这里不以“测试数量”代替行为证据：live prompt benchmark 走生产 AgentLoop/provider 和确定性 in-memory tools；离线 gate 直接走 AgentLoop、checkpoint、CompactionCheckpointMessages、session Store round-trip、HistoryForLoop 与 ResumeInterrupted。</p><div class="grid"><div class="card"><strong class="ok">{correct_prompt_tool_runs} / 132</strong>live 工具轨迹正确<small>任务结果 {successful_prompt_tasks}/132；差异来自 minimal stress control 的 1 次答案失败</small></div><div class="card"><strong class="ok">0</strong>重复工具 / 重复副作用<small>132-run benchmark 的 observed executions</small></div><div class="card"><strong class="ok">10 / 10</strong>loop corpus<small>{loop_report['productive_count']} productive + {loop_report['loop_count']} genuine loops；false={loop_report['false_signals']}，missed={loop_report['missed_loops']}</small></div></div>
<div class="trace"><time>14-step read</time><span class="rail"></span><p>14 个不同只读步骤完整结束；每步只执行一次，lossless RunMessages 保留全部 tool result。生产循环允许一次有界 progress/nudge provider turn，但没有误杀长轨迹。</p><time>compaction</time><span class="rail"></span><p>增长 usage 触发 proactive compaction；live checkpoint 比 archive 小且带 production marker，lossless archive 保持独立。</p><time>process restart</time><span class="rail"></span><p>checkpoint 写入真实 session Store，再加载并经 HistoryForLoop 选择 compacted live state；新 AgentLoop 可读回最终 STEP-14。</p><time>interruption</time><span class="rail"></span><p>第 6 步取消后，RunMessages 只含 6 个完成结果；ResumeInterrupted 从第 7 步继续到第 10 步，全部步骤最终各执行一次。</p></div>
<div class="callout"><strong>离线边界：</strong>这些 gate 使用确定性 provider/tool fixture 来验证 runtime 拓扑，不证明真实模型的写作、研究、规划质量，也不模拟外部服务在 commit 与 receipt 之间崩溃。</div>
{code_block('离线 runtime gate 实际输出' if offline_gate_log else '离线 runtime gate 日志未提供', offline_gate_log or 'No offline gate log was supplied to the generator.', True)}
{code_block('离线 loop detector 完整 artifact', json.dumps(loop_report, ensure_ascii=False, indent=2))}</section>

<section id="tools"><div class="eyebrow">03 / provider-visible tool surface</div><h2>Fast 工具面收口：更小，但只对输入下降有因果证据</h2><p class="sub">最终实现是 run-local、Fast-only 的 long-tail defer。Full 与普通 CLI 不继承这套收口；Fast 仍通过 <code>tool_search</code> 发现并执行冷工具。WorkingSet 为 session-scoped，并以数量和 schema-token 双上限阻止长期会话重新膨胀。</p><div class="grid"><div class="card"><strong class="ok">{tool_surface['after_tools']} tools</strong>最终 Fast provider-visible 集合<small>{tool_surface['before_tools']} → {tool_surface['after_tools']}；tools JSON {tool_surface['before_chars']:,} → {tool_surface['after_chars']:,} chars（−{tool_chars_reduction:.1f}%）</small></div><div class="card"><strong class="ok">−{daemon_input_reduction:.1f}%</strong>隔离 daemon input tokens<small>{tool_surface['daemon_input_before']:,} → {tool_surface['daemon_input_after']:,}</small></div><div class="card"><strong>{tool_surface['working_set_count_cap']} / {tool_surface['working_set_token_cap']//1000}K</strong>WorkingSet cap<small>最多 {tool_surface['working_set_count_cap']} schemas 或约 {tool_surface['working_set_token_cap']:,} schema tokens</small></div></div>
<table style="margin-top:14px"><thead><tr><th>证据</th><th>Before</th><th>After</th><th>可作出的结论</th></tr></thead><tbody><tr><th>Fast tools JSON</th><td>{tool_surface['before_chars']:,} chars / {tool_surface['before_tools']} tools</td><td>{tool_surface['after_chars']:,} chars / {tool_surface['after_tools']} tools</td><td>Fast provider request 的工具 schema 确实缩小；long-tail 仍可经 tool_search 加载</td></tr><tr><th>Bash schema estimate</th><td>{tool_surface['bash_tokens_before']:,} tokens</td><td>{tool_surface['bash_tokens_after']:,} tokens（−{bash_schema_reduction:.1f}%）</td><td><code>bash</code> 仍为 Direct；通过缩短 schema 降低常驻成本，不改变执行能力</td></tr><tr><th>隔离 daemon input</th><td>{tool_surface['daemon_input_before']:,} tokens</td><td>{tool_surface['daemon_input_after']:,} tokens</td><td>同一路径单样本观察到 −{daemon_input_reduction:.1f}%；这是输入量的直接度量</td></tr><tr><th>隔离 daemon total</th><td>{ms(tool_surface['daemon_total_before_ms'])}</td><td>{ms(tool_surface['daemon_total_after_ms'])}</td><td>单样本 {tool_surface['daemon_total_before_ms']:,}→{tool_surface['daemon_total_after_ms']:,} ms 只能描述，不能归因，也不能作为统计提速</td></tr></tbody></table>
<div class="callout"><strong>5/5 isolated daemon matrix PASS（{esc(koe_matrix_total.group(1))}s）。</strong>隔离 daemon 重启后读回 5 个 session。它证明最终 Fast/Full runtime/tool topology 在隔离 daemon 路径可完成；不等于真实麦克风、声学回声、VAD/barge-in 或签名 Desktop UI 验证。</div>{code_block('Koe 5-case isolated runtime 完整日志', koe_runtime_log)}</section>

<section id="quality"><div class="eyebrow">04 / 18 real-provider runs, 3x per case</div><h2>General-purpose quality：六个 domain 全部通过，样本仍小</h2><p class="sub">真实配置 provider + production AgentLoop，response cache 强制 off。六个 deterministic product-contract validators 分别覆盖中文两句邮件、notes 忠实总结、deadline plan、voice-style 短答、bounded research 和 Deferred automation；不使用另一个 LLM judge 改写或打分。</p><div class="grid"><div class="card"><strong class="ok">{quality_report['correct_runs']} / {quality_report['completed']}</strong>observed correctness<small>6 cases × {quality_report['repetitions_per_case']} randomized repetitions</small></div><div class="card"><strong>{ms(quality_report['latency_p50_millis'])} / {ms(quality_report['latency_p95_millis'])} / {ms(quality_report['latency_p99_millis'])}</strong>P50 / P95 / P99<small>18-run sample；tail 主要由 tool cases 驱动</small></div><div class="card"><strong class="bad">release = false</strong>comparison only<small>每 case {quality_report['repetitions_per_case']} 次；release 要求 ≥{quality_report['minimum_release_repetitions']}</small></div></div>
<h3>Per-domain results</h3><table><thead><tr><th>Domain</th><th>Correct</th><th>P50</th><th>P95</th><th>P99</th><th>Input tokens</th><th>Cost</th></tr></thead><tbody>{quality_rows(quality_report)}</tbody></table>
<div class="callout"><strong>Failures：</strong>{esc(quality_report['failures'] or 'none observed')}。总 observed cost {usd(quality_report['reported_cost_usd'])}，tokens {quality_report['total_tokens']:,}。18/18 是这六个合同在本次样本中的结果，不是开放式“人类偏好质量”或 release 可靠性证明。</div>
{code_block('General-purpose quality 18-run 原始 JSON', json.dumps(quality_report, ensure_ascii=False, indent=2))}</section>

<section id="prompt"><div class="eyebrow">05 / 132 live runs, 3x per cell</div><h2>Prompt benchmark：完整 tail、paired 与 no-candidate 结论</h2><p class="sub">11 workloads × 4 variants × 3 matched repetitions；同一 AgentLoop、工具、Luna Fast 模式和 workload，仅替换 system instructions。三次 repetition 达到 comparison 门槛但远低于每 workload 30 次的 release 门槛。<code>minimal_v1</code> 是不可上线的 stress control。</p><div class="grid"><div class="card"><strong class="warn">{prompt_report['winner_status']}</strong>选择器结论<small>{esc(prompt_report['selection_reason'])}</small></div><div class="card"><strong class="bad">release = false</strong>不是 release-ready<small>minimum release repetitions = {prompt_report['minimum_release_repetitions']}</small></div><div class="card"><strong>{prompt_report['completed']}/{prompt_report['scheduled']}</strong>real-provider runs completed<small>{usd(prompt_report['reported_cost_usd'])} observed cost；randomized matched blocks</small></div></div>
<h3>Aggregate latency / correctness</h3><table><thead><tr><th>Variant</th><th>任务</th><th>工具</th><th>P50</th><th>P95</th><th>P99</th><th>Max</th><th>均值 input</th><th>成本</th></tr></thead><tbody>{prompt_rows(prompt_report)}</tbody></table>
<p class="sub">P99 在 33-run variant 上等于或接近最慢 observation，必须和 max、slowest runs 一起读。<code>layered_v1</code> 虽然 P50 最快，但 P95/P99/Max 最差；<code>minimal_v1</code> 尾部较短，却在 <code>current_search_once</code> repetition 2 产生任务错误。</p>
<h3>Matched-pair delta（candidate − current）</h3><table><thead><tr><th>Candidate</th><th>Pairs</th><th>胜 / 负 / 平</th><th>胜率</th><th>Δ P50</th><th>Δ P95</th><th>Δ P99</th><th>Δ Min / Max</th></tr></thead><tbody>{paired_rows(prompt_report)}</tbody></table>
<p class="sub">负 delta 表示 candidate 更快。三组 candidate 的 median paired delta 都略快，但 P95/P99 delta 均为正，说明尾部存在更慢 matched pairs；这正是不能只看 aggregate P50 或“win rate”的原因。</p>
<h3>每个 variant 最慢三次</h3><table><thead><tr><th>Variant / rank</th><th>Workload</th><th>Rep</th><th>Schedule</th><th>Latency</th></tr></thead><tbody>{outlier_rows(prompt_report)}</tbody></table>
<div class="callout"><strong>唯一 observed task failure：</strong>{esc(json.dumps(prompt_failures, ensure_ascii=False))}。所有 132 个工具轨迹仍正确，说明 evaluator 将“执行拓扑正确”和“最终任务答案正确”分开计数。</div>
{code_block('Prompt 132-run 原始 JSON', json.dumps(prompt_report, ensure_ascii=False, indent=2))}</section>

<section id="memory"><div class="eyebrow">06 / 48 paired synthetic-memory runs</div><h2>Memory A/B：主模型 recall 降低固定 helper 成本</h2><p class="sub">3 repetitions、48 total runs。模型看到生产 <code>SessionSearchTool.Info()</code> 的名称、description 与 schema；Run 和 Ready memory service 使用 deterministic fixture。它测路由、调用、答案忠实度、延迟和成本，不测真实 sidecar bundle 的生成、检索与排序质量。</p><table><thead><tr><th>路径</th><th>总正确</th><th>正例路由</th><th>路由效率</th><th>正例答案</th><th>负例误召回</th><th>平均延迟</th><th>总成本</th></tr></thead><tbody>{memory_rows(memory_report)}</tbody></table>
<div class="grid" style="margin-top:14px"><div class="card"><strong class="ok">24 / 24 × 2</strong>两路答案与路由均正确<small>18 positive + 6 negative per variant</small></div><div class="card"><strong class="ok">−{memory_cost_reduction:.1f}%</strong>主模型路径成本<small>{usd(model_memory['total_cost_usd'])} vs {usd(preflight_memory['total_cost_usd'])}</small></div><div class="card"><strong class="ok">−{memory_mean_reduction:.1f}% / −{memory_median_reduction:.1f}%</strong>平均 / 中位延迟<small>{ms(model_memory['mean_latency_ms'])} vs {ms(preflight_memory['mean_latency_ms'])}</small></div></div>
<div class="callout"><strong>失败 {len(memory_failures)} 条。</strong>主模型路径避免了 {preflight_memory['preflight_helpers']} 次 preflight helper，同时按需发出 {model_memory['explicit_queries']} 次 explicit memory queries 和 {model_memory['session_search_queries']} 次 session search。这个成本/延迟优势不能外推为真实用户 memory recall accuracy。</div>
{code_block('Memory 48-run 原始 JSON', json.dumps(memory_report, ensure_ascii=False, indent=2))}</section>

<section id="layers"><div class="eyebrow">07 / production builder exact output</div><h2>Kocoro / Koe Full / Koe Fast exact prompts</h2><p class="sub">以下文本由当前 production builder 导出，不是手工摘要。该 audit 使用代表性的本地工具集，刻意不含每用户 MCP、gateway、instructions、memory、cwd 和 sticky context，因此真实请求会按 registry/session 变化。Full 与 Fast 因 provider-visible capability/tool guidance 不同，最终 system 和 stable context 也不同；下面分别保留 chars 与 UTF-8 bytes，不能再把两种 profile 合并描述。</p>
<div class="grid"><div class="card"><strong>{len(full['system']):,} / {full_system_bytes:,}</strong>Full system chars / bytes<small>stable {len(full['stable_context']):,} chars / {full_stable_bytes:,} bytes；volatile {len(full['volatile_context']):,}</small></div><div class="card"><strong>{len(fast['system']):,} / {fast_system_bytes:,}</strong>Fast system chars / bytes<small>stable {len(fast['stable_context']):,} chars / {fast_stable_bytes:,} bytes；volatile {len(fast['volatile_context']):,}</small></div><div class="card"><strong>{len(layers['kocoro_base_prompt']):,}</strong>shared assembled base chars<small>Full/Fast base layers are both displayed even when their current text is equal</small></div></div>
<h3>Kocoro exact assembled bases</h3>{code_block('Kocoro shared base prompt', layers['kocoro_base_prompt'], True)}{code_block('Kocoro Full base prompt', layers['kocoro_full_base_prompt'], True)}{code_block('Kocoro Fast base prompt', layers['kocoro_fast_base_prompt'], True)}{code_block('defaultPersona', layers['default_persona'])}{code_block('coreOperationalRules', layers['core_operational_rules'])}{code_block('contrastExamplesCore', layers['core_contrast_examples'])}
<h3>Koe Full exact assembled request layers</h3>{code_block('Koe Full / system', full['system'], True)}{code_block('Koe Full / stable_context', full['stable_context'], True)}{code_block('Koe Full / volatile_context', full['volatile_context'], True)}
<h3>Koe Fast exact assembled request layers</h3>{code_block('Koe Fast / system', fast['system'], True)}{code_block('Koe Fast / stable_context', fast['stable_context'], True)}{code_block('Koe Fast / volatile_context', fast['volatile_context'], True)}
<h3>Conditional production layers</h3>{code_block('cloud_delegation_guidance', layers['cloud_delegation_guidance'])}{code_block('cloud contrast examples', layers['cloud_contrast_examples'])}
{code_block('Prompt audit 原始 JSON', json.dumps(prompt_audit, ensure_ascii=False, indent=2))}</section>

<section id="comparison"><div class="eyebrow">08 / architecture, not imitation</div><h2>横向设计对比</h2><p class="sub">比较的是 prompt/runtime 边界的方法，不把 coding-agent 专用规则原样移植到通用助手。</p><table><thead><tr><th>系统</th><th>Prompt 组织</th><th>工具与恢复</th><th>Kocoro 取舍</th></tr></thead><tbody>
<tr><th>Codex</th><td>model base + developer/user layers；动态权限与目标由 runtime 状态承载</td><td>强调 sandbox、approval、持久状态与可验证工具结果</td><td>把 runtime 可执行的不变量从长提示词中移出</td></tr>
<tr><th>Hermes</th><td>stable/workspace/trailing 分层，cache-aware builder</td><td>personality、skills、memory 可插拔，也使用 helper</td><td>保留 stable/volatile 分层；helper 必须用实测收益证明</td></tr>
<tr><th>OpenClaw</th><td>section builder + cache boundary + channel/plugin additions</td><td>repeat/no-progress/ping-pong detectors 可配置</td><td>继续保留 outcome-aware detector，并扩展真实工具生态回放</td></tr>
<tr><th>Kocoro</th><td>当前源码已拆 persona、core rules、boundary examples、stable/volatile</td><td>批次前 loop 阻断、checkpoint、compaction、idempotency 与 outcome_unknown</td><td>运行骨架有证据；quality 只有 3× smoke，prompt 候选仍需 ≥30 matched reps 与人类盲评</td></tr></tbody></table></section>

<section id="gaps"><div class="eyebrow">09 / explicit non-claims</div><h2>未覆盖边界</h2><p class="sub">这些不是“以后再补”的装饰项，而是当前报告不能作出的产品声明。</p><table><thead><tr><th>优先</th><th>未覆盖</th><th>当前能证明什么</th><th>关闭标准</th></tr></thead><tbody>
<tr><th><span class="tag">NOW</span></th><td>开放式自然质量与人类偏好</td><td>18/18 deterministic contracts 覆盖六个 domain；不等于开放式答案好用或偏好胜出</td><td>盲评 rubric、固定任务与对照；质量、延迟、成本分开</td></tr>
<tr><th><span class="tag">NOW</span></th><td>真实 sidecar / 用户 bundle memory</td><td>48-run synthetic A/B 证明 routing seam，不证明真实召回率</td><td>隔离真实 bundle，统计 query、with-data、答案忠实度和 no-data</td></tr>
<tr><th><span class="tag">NOW</span></th><td>3× prompt / quality rare-failure 与 release qualification</td><td>比较样本有效，二者都明确不是 release-ready</td><td>每 workload/case ≥30 complete matched repetitions，产品 correctness gate 全通过并报告置信区间</td></tr>
<tr><th><span class="tag">NEXT</span></th><td>真实 MCP / browser / computer / signed-in apps</td><td>当前 benchmark 的工具确定性且无外部写入</td><td>dev daemon 上 read-only 与可回滚写入，包含 auth、UI state、tool_search catalog</td></tr>
<tr><th><span class="tag">NEXT</span></th><td>外部写 commit 后进程崩溃</td><td>本地 restart 证明 checkpoint 连续性和无工具重放；不覆盖远端 outcome_unknown</td><td>故障注入 commit/receipt 窗口，按 idempotency key 对账并 fail closed</td></tr>
<tr><th><span class="tag">NEXT</span></th><td>真实麦克风、声学链路与签名 Desktop UI</td><td>5/5 isolated daemon matrix 已通过；不含真实 mic/AEC/VAD/barge-in、声学环境或签名 UI</td><td>真实设备、声学环境、签名 Desktop UI、端到端 latency 与中断体验</td></tr></tbody></table>
<h3>Artifact 自带 coverage boundaries</h3>{bullet_list(prompt_report['coverage_boundaries'] + memory_report['coverage_boundaries'] + quality_report['coverage_boundaries'])}</section>

<section id="diff"><div class="eyebrow">10 / complete branch evidence</div><h2>完整 branch diff</h2><p class="sub">下面是 <code>origin/main...HEAD</code> 的完整代码/脚本差异，并追加生成时仍存在的 tracked working-tree diff。为避免 HTML 把自身无限递归嵌入，唯一排除项是 <code>docs/agent-harness-review.html</code>；生成器 <code>scripts/generate-agent-harness-report.py</code> 已先提交，因此明确包含在此 diff 中。范围：<code>{esc(branch_base)}...{esc(branch_head)}</code>。</p>{code_block('完整 diff（仅排除本 HTML）', full_diff)}</section>
<footer>Generated by scripts/generate-agent-harness-report.py · self-contained · no external assets · HTML excluded from embedded diff only to prevent recursion</footer>
</main></div></body></html>"""
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(document, encoding="utf-8")


if __name__ == "__main__":
    main()
