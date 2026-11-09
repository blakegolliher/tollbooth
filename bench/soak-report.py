#!/usr/bin/env python3
# The soak's verdict (bench/soak.sh calls it at the end; run it by hand over a finished or running
# soak directory to regenerate soak-report.txt): RSS slope over the second half, and every error
# event attributed to the chaos window it landed in or called unexplained (D-056).
#
#   bench/soak-report.py bench/results/soak-<date>
import csv, sys, os
out = sys.argv[1]
rows = list(csv.DictReader(open(os.path.join(out, "soak.csv"))))
if len(rows) < 3:
    print("too few samples"); sys.exit(1)
rss = [float(r["rss_mib"]) for r in rows]
half = rows[len(rows)//2:]
xs = [float(r["elapsed_s"]) / 3600 for r in half]; ys = [float(r["rss_mib"]) for r in half]
n = len(xs); mx = sum(xs)/n; my = sum(ys)/n
slope = sum((x-mx)*(y-my) for x, y in zip(xs, ys)) / (sum((x-mx)**2 for x in xs) or 1)
# Errors by chaos window: proxy errors, backend 5xx, backend transport errors, and flush errors
# whose delta lands in a sample taken during a chaos step (or the sample right after it) belong to
# that step; a delta with no step in effect in this sample or the previous one is unexplained.
unexplained = 0; explained = {}; windows = {}
for prev, cur in zip(rows, rows[1:]):
    d = 0
    for col in ("errors_total", "backend_5xx_total", "backend_errors_total", "flush_errors_total"):
        d += int(cur[col]) - int(prev[col])
    a, b = prev["chaos"], cur["chaos"]
    win = b if a in ("none", b) else (a if b == "none" else f"{a}/{b}")  # a/b: the sample spans two windows
    if win != "none":
        windows[win] = windows.get(win, 0) + int(cur["requests_total"]) - int(prev["requests_total"])
    if d == 0: continue
    if win == "none": unexplained += d
    else: explained[win] = explained.get(win, 0) + d
chaos_lines = open(os.path.join(out, "chaos.log")).read().splitlines() if os.path.exists(os.path.join(out, "chaos.log")) else []
rep = [
    f"samples: {len(rows)} over {float(rows[-1]['elapsed_s'])/3600:.2f} h",
    f"requests: {rows[-1]['requests_total']} total, {rows[-1]['throttled_total']} throttled, reloads ok {rows[-1]['reloads_ok']} err {rows[-1]['reloads_err']}",
    f"rss: min {min(rss):.0f} MiB, max {max(rss):.0f} MiB, final {rss[-1]:.0f} MiB; slope over the second half {slope:+.2f} MiB/h",
    f"heap: final {rows[-1]['heap_mib']} MiB; goroutines final {rows[-1]['goroutines']}; limiter keys {rows[-1]['limiter_keys']}; showback tuples {rows[-1]['showback_tuples']}",
    f"chaos steps: {sum(1 for l in chaos_lines if ' begin ' in l)}",
    "error events by chaos window (proxy errors + backend 5xx + backend transport errors + flush errors, over completed requests in the window):",
    *([f"  {k}: {explained.get(k, 0)} over {windows[k]} requests" for k in sorted(windows)] or ["  none"]),
    f"unexplained errors (outside every chaos window): {unexplained}",
    "verdict: " + ("flat memory, zero unexplained errors" if abs(slope) < 5 and unexplained == 0 else ("run too short for a slope (under 1 h); " if float(rows[-1]["elapsed_s"]) < 3600 else "") + ("zero unexplained errors" if unexplained == 0 else f"{unexplained} UNEXPLAINED ERRORS")),
]
open(os.path.join(out, "soak-report.txt"), "w").write("\n".join(rep) + "\n")
print("\n".join(rep))
