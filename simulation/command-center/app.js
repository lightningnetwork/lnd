/* ============================================================
   GEPA Routing Optimizer — Command Center
   Vanilla JS. Hand-rolled SVG charts, no build step, no CDN JS.
   ============================================================ */

const SVGNS = "http://www.w3.org/2000/svg";
const COL = {
  amber:  "#f5a623",
  blue:   "#3987e5",
  orange: "#d95926",
  aqua:   "#199e70",
  yellow: "#c98500",
  ink3:   "#6d7889",
  grid:   "rgba(255,255,255,0.08)",
  base:   "#2c3646",
};

const $  = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const el = (tag, attrs = {}, kids = []) => {
  const isSvg = ["svg","g","path","line","circle","rect","text","polyline","defs"].includes(tag);
  const n = isSvg ? document.createElementNS(SVGNS, tag) : document.createElement(tag);
  for (const k in attrs) {
    if (k === "class") n.setAttribute("class", attrs[k]);
    else if (k === "text") n.textContent = attrs[k];
    else if (k === "html") n.innerHTML = attrs[k];
    else n.setAttribute(k, attrs[k]);
  }
  (Array.isArray(kids) ? kids : [kids]).forEach(c => c && n.appendChild(c));
  return n;
};
const fmt = n => n.toLocaleString("en-US");
const sat = msatOrCount => msatOrCount; // labels are pre-bucketed

/* ============================================================
   Navigation: scroll-spy + mobile menu
   ============================================================ */
function initNav() {
  const links = $$("#nav a");
  const map = {};
  links.forEach(a => map[a.getAttribute("href").slice(1)] = a);
  const obs = new IntersectionObserver(entries => {
    entries.forEach(e => {
      if (e.isIntersecting) {
        links.forEach(l => l.classList.remove("active"));
        map[e.target.id]?.classList.add("active");
      }
    });
  }, { rootMargin: "-45% 0px -50% 0px", threshold: 0 });
  $$("section").forEach(s => obs.observe(s));

  const rail = $("#rail"), scrim = $("#scrim");
  const close = () => { rail.classList.remove("open"); scrim.classList.remove("open"); };
  $("#burger")?.addEventListener("click", () => { rail.classList.toggle("open"); scrim.classList.toggle("open"); });
  scrim?.addEventListener("click", close);
  links.forEach(a => a.addEventListener("click", close));
}

/* ============================================================
   Stat tiles
   ============================================================ */
function tile({ k, v, unit, delta, deltaClass, hi }) {
  const t = el("div", { class: "tile" + (hi ? " hi" : "") });
  t.appendChild(el("div", { class: "tk", text: k }));
  const val = el("div", { class: "tv mono" });
  val.appendChild(document.createTextNode(v));
  if (unit) val.appendChild(el("span", { class: "unit", text: unit }));
  t.appendChild(val);
  if (delta) {
    const arrow = deltaClass === "up" ? "▲" : deltaClass === "down" ? "▼" : "•";
    t.appendChild(el("div", { class: "td " + deltaClass, html: `<span>${arrow}</span> ${delta}` }));
  }
  return t;
}

/* ============================================================
   Line chart — score over iterations
   ============================================================ */
function scoreChart(mount, run) {
  const its = run.iterations;
  const W = 720, H = 320, PAD = { t: 18, r: 46, b: 34, l: 44 };
  const iw = W - PAD.l - PAD.r, ih = H - PAD.t - PAD.b;

  const xs = its.map(d => d.i);
  const xMin = Math.min(...xs), xMax = Math.max(...xs);
  const all = its.flatMap(d => [d.candidate_score, d.best_score]).concat(run.seed_score);
  const step = 0.05;
  let yMin = Math.floor((Math.min(...all) - 0.01) / step) * step;
  let yMax = Math.ceil((Math.max(...all) + 0.01) / step) * step;
  yMin = Math.max(0, yMin);

  const X = i => PAD.l + (i - xMin) / (xMax - xMin) * iw;
  const Y = v => PAD.t + (1 - (v - yMin) / (yMax - yMin)) * ih;

  const svg = el("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": "Score over iterations" });

  // y gridlines + labels (round 0.05 steps)
  const ticks = Math.round((yMax - yMin) / step);
  for (let i = 0; i <= ticks; i++) {
    const v = yMin + step * i;
    const y = Y(v);
    svg.appendChild(el("line", { x1: PAD.l, y1: y, x2: W - PAD.r, y2: y, stroke: COL.grid, "stroke-width": 1 }));
    svg.appendChild(el("text", { x: PAD.l - 10, y: y + 3.5, "text-anchor": "end", fill: COL.ink3, "font-size": 10.5, "font-family": "var(--font-mono)", text: v.toFixed(2) }));
  }
  // x labels (every other)
  its.forEach((d, idx) => {
    if (idx % 3 !== 0 && idx !== its.length - 1) return;
    svg.appendChild(el("text", { x: X(d.i), y: H - PAD.b + 18, "text-anchor": "middle", fill: COL.ink3, "font-size": 10.5, "font-family": "var(--font-mono)", text: d.i }));
  });
  svg.appendChild(el("text", { x: PAD.l + iw / 2, y: H - 2, "text-anchor": "middle", fill: COL.ink3, "font-size": 10, "font-family": "var(--font-mono)", "letter-spacing": "0.12em", text: "ITERATION" }));

  // seed baseline (dashed reference)
  const by = Y(run.seed_score);
  svg.appendChild(el("line", { x1: PAD.l, y1: by, x2: W - PAD.r, y2: by, stroke: COL.ink3, "stroke-width": 1.5, "stroke-dasharray": "5 5", opacity: 0.8 }));
  svg.appendChild(el("text", { x: W - PAD.r + 4, y: by + 3.5, fill: COL.ink3, "font-size": 10, "font-family": "var(--font-mono)", text: "seed" }));

  const line = (accessor, color, width, dash) => {
    const pts = its.map(d => `${X(d.i)},${Y(accessor(d))}`).join(" ");
    svg.appendChild(el("polyline", { points: pts, fill: "none", stroke: color, "stroke-width": width, "stroke-linejoin": "round", "stroke-linecap": "round", ...(dash ? { "stroke-dasharray": dash } : {}) }));
  };

  // candidate (thin blue) then best (amber, thicker) on top
  line(d => d.candidate_score, COL.blue, 1.8);
  its.forEach(d => svg.appendChild(el("circle", { cx: X(d.i), cy: Y(d.candidate_score), r: 3, fill: COL.blue, stroke: "#10161f", "stroke-width": 1.5 })));

  line(d => d.best_score, COL.amber, 2.6);
  its.forEach(d => svg.appendChild(el("circle", { cx: X(d.i), cy: Y(d.best_score), r: 3.4, fill: COL.amber, stroke: "#10161f", "stroke-width": 1.5 })));

  // end label for best
  const last = its[its.length - 1];
  svg.appendChild(el("text", { x: X(last.i) + 6, y: Y(last.best_score) - 8, fill: COL.amber, "font-size": 12, "font-weight": 600, "font-family": "var(--font-mono)", text: last.best_score.toFixed(2) }));

  // hover layer
  const cross = el("line", { x1: 0, y1: PAD.t, x2: 0, y2: H - PAD.b, stroke: "rgba(245,166,35,0.5)", "stroke-width": 1, opacity: 0 });
  const focC = el("circle", { r: 5, fill: "none", stroke: COL.amber, "stroke-width": 2, opacity: 0 });
  svg.appendChild(cross); svg.appendChild(focC);

  mount.appendChild(svg);
  const tip = el("div", { class: "tip" });
  mount.appendChild(tip);

  const hit = el("rect", { x: PAD.l, y: PAD.t, width: iw, height: ih, fill: "transparent", style: "cursor:crosshair" });
  svg.appendChild(hit);

  const toLocal = evt => {
    const pt = svg.createSVGPoint(); pt.x = evt.clientX; pt.y = evt.clientY;
    return pt.matrixTransform(svg.getScreenCTM().inverse());
  };
  hit.addEventListener("mousemove", evt => {
    const p = toLocal(evt);
    let best = its[0], bd = Infinity;
    its.forEach(d => { const dx = Math.abs(X(d.i) - p.x); if (dx < bd) { bd = dx; best = d; } });
    const cx = X(best.i);
    cross.setAttribute("x1", cx); cross.setAttribute("x2", cx); cross.setAttribute("opacity", 1);
    focC.setAttribute("cx", cx); focC.setAttribute("cy", Y(best.best_score)); focC.setAttribute("opacity", 1);
    tip.classList.add("show");
    tip.innerHTML = `<div class="tt-h">Iteration ${best.i}</div>
      <div class="tt-r"><span class="lab"><span class="sw" style="background:${COL.amber}"></span>best</span><span class="val">${best.best_score.toFixed(3)}</span></div>
      <div class="tt-r"><span class="lab"><span class="sw" style="background:${COL.blue}"></span>candidate</span><span class="val">${best.candidate_score.toFixed(3)}</span></div>
      <div class="tt-note">${best.note}</div>`;
    // position tooltip in DOM coords
    const rect = mount.getBoundingClientRect();
    const px = (cx / W) * rect.width;
    const py = (Y(best.best_score) / H) * rect.height;
    const left = Math.min(Math.max(px + 14, 4), rect.width - tip.offsetWidth - 4);
    tip.style.left = left + "px";
    tip.style.top = Math.max(py - 10, 4) + "px";
  });
  hit.addEventListener("mouseleave", () => {
    cross.setAttribute("opacity", 0); focC.setAttribute("opacity", 0); tip.classList.remove("show");
  });
}

/* ============================================================
   Params table — seed vs best
   ============================================================ */
function paramsTable(mount, run) {
  const seed = run.seed_params, best = run.best_candidate;
  const rows = [
    { g: "core" },
    { k: "estimator", path: ["estimator"], fmt: v => v },
    { k: "attempt_cost_msat", path: ["attempt_cost_msat"], fmt: v => fmt(v) },
    { k: "attempt_cost_ppm", path: ["attempt_cost_ppm"], fmt: v => fmt(v) },
    { k: "min_probability", path: ["min_probability"], fmt: v => v },
    { g: "apriori" },
    { k: "hop_probability", path: ["apriori", "hop_probability"], fmt: v => v },
    { k: "weight", path: ["apriori", "weight"], fmt: v => v },
    { k: "penalty_half_life_sec", path: ["apriori", "penalty_half_life_sec"], fmt: v => fmt(v) },
    { g: "bimodal" },
    { k: "scale_msat", path: ["bimodal", "scale_msat"], fmt: v => fmt(v) },
    { k: "node_weight", path: ["bimodal", "node_weight"], fmt: v => v },
    { k: "decay_time_sec", path: ["bimodal", "decay_time_sec"], fmt: v => fmt(v) },
  ];
  const get = (o, p) => p.reduce((a, k) => a?.[k], o);

  mount.innerHTML = "";
  const thead = el("thead");
  thead.appendChild(el("tr", {}, [
    el("th", { text: "parameter" }),
    el("th", { text: "seed", style: "text-align:right" }),
    el("th", { text: "best", style: "text-align:right" }),
  ]));
  mount.appendChild(thead);
  const tb = el("tbody");
  rows.forEach(r => {
    if (r.g) { tb.appendChild(el("tr", { class: "group" }, el("td", { colspan: 3, text: r.g }))); return; }
    const sv = get(seed, r.path), bv = get(best, r.path);
    const changed = String(sv) !== String(bv);
    const tr = el("tr", { class: changed ? "changed" : "" });
    tr.appendChild(el("td", { class: "k", text: r.k }));
    tr.appendChild(el("td", { class: "num seed", text: r.fmt(sv) }));
    tr.appendChild(el("td", { class: "num best", text: r.fmt(bv) }));
    tb.appendChild(tr);
  });
  mount.appendChild(tb);
}

/* ============================================================
   Horizontal bar chart (corpus categories)
   ============================================================ */
function barChart(mount, data, { color, total, unit } = {}) {
  mount.innerHTML = "";
  const max = Math.max(...data.map(d => d.value));
  const sum = total ?? data.reduce((a, d) => a + d.value, 0);
  const palette = Array.isArray(color) ? color : null;
  data.forEach((d, i) => {
    const row = el("div", { class: "barrow" });
    row.appendChild(el("div", { class: "blab", text: d.label }));
    const track = el("div", { class: "bartrack" });
    const fill = el("div", { class: "barfill" });
    const c = palette ? palette[i % palette.length] : (color || COL.amber);
    fill.style.background = `linear-gradient(90deg, ${c}, ${c}cc)`;
    fill.style.width = "0%";
    track.appendChild(fill);
    row.appendChild(track);
    const pct = ((d.value / sum) * 100).toFixed(0);
    row.appendChild(el("div", { class: "bval", html: `${fmt(d.value)} <small>${pct}%</small>` }));
    mount.appendChild(row);
    requestAnimationFrame(() => { fill.style.width = (d.value / max * 100) + "%"; });
  });
}

/* ============================================================
   Evolution — prior vs evolved comparison
   ============================================================ */
function renderCompare(mount, run) {
  const s = run.stats;
  const metric = (k, v) => `<div class="cmp-metric"><span class="m-k">${k}</span><span class="m-v">${v}</span></div>`;
  const prior = `
    <div class="cmp-col prior">
      <div class="cmp-tag">Prior paradigm</div>
      <div class="cmp-name">lnd defaults</div>
      <div class="cmp-desc">Dijkstra + apriori mission control</div>
      ${metric("score", run.seed_score.toFixed(2))}
      ${metric("success rate", (s.seed_success_rate * 100).toFixed(0) + "%")}
      ${metric("attempts / payment", s.seed_attempts_per_payment.toFixed(1))}
      ${metric("fee (ppm)", fmt(s.seed_fee_ppm))}
    </div>`;
  const dScore = (run.best_score - run.seed_score).toFixed(2);
  const arrow = `
    <div class="cmp-col mid"><div class="cmp-arrow">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M5 12h14M13 6l6 6-6 6"/></svg>
      <span class="adelta">+${dScore} score</span>
    </div></div>`;
  const evolved = `
    <div class="cmp-col evolved">
      <div class="cmp-tag">Evolved candidate · c13</div>
      <div class="cmp-name">GEPA best</div>
      <div class="cmp-desc">bimodal estimator, tuned attempt cost</div>
      ${metric("score", run.best_score.toFixed(2))}
      ${metric("success rate", (s.best_success_rate * 100).toFixed(0) + "%")}
      ${metric("attempts / payment", s.best_attempts_per_payment.toFixed(1))}
      ${metric("fee (ppm)", fmt(s.best_fee_ppm))}
    </div>`;
  mount.innerHTML = prior + arrow + evolved;
}

/* ============================================================
   Evolution — lineage DAG
   ============================================================ */
function lineageDag(mount, cands, onSelect) {
  const byId = {}; cands.forEach(c => byId[c.id] = c);
  const kids = {}; cands.forEach(c => { if (c.parent != null) (kids[c.parent] ??= []).push(c.id); });
  Object.values(kids).forEach(a => a.sort((x, y) => x - y));
  const roots = cands.filter(c => c.parent == null).map(c => c.id);

  // depth
  const depth = {};
  const setDepth = (id, d) => { depth[id] = d; (kids[id] || []).forEach(k => setDepth(k, d + 1)); };
  roots.forEach(r => setDepth(r, 0));

  // y via leaf assignment (post-order)
  const y = {}; let leaf = 0;
  const assignY = id => {
    const ch = kids[id] || [];
    if (!ch.length) { y[id] = leaf++; return; }
    ch.forEach(assignY);
    y[id] = (y[ch[0]] + y[ch[ch.length - 1]]) / 2;
  };
  roots.forEach(assignY);

  const NW = 104, NH = 34, xGap = 138, yGap = 50, mx = 16, my = 20;
  const maxD = Math.max(...Object.values(depth));
  const W = mx * 2 + maxD * xGap + NW;
  const H = my * 2 + (leaf - 1) * yGap + NH;
  const X = id => mx + depth[id] * xGap;
  const Y = id => my + y[id] * yGap;

  const svg = el("svg", { viewBox: `0 0 ${W} ${H}`, width: W, height: H, style: `min-width:${W}px`, role: "img", "aria-label": "Candidate lineage" });

  // edges first
  cands.forEach(c => {
    if (c.parent == null) return;
    const x1 = X(c.parent) + NW, y1 = Y(c.parent) + NH / 2;
    const x2 = X(c.id), y2 = Y(c.id) + NH / 2;
    const mxp = (x1 + x2) / 2;
    const acc = c.accepted;
    svg.appendChild(el("path", {
      d: `M ${x1} ${y1} C ${mxp} ${y1}, ${mxp} ${y2}, ${x2} ${y2}`,
      fill: "none", stroke: acc ? "rgba(245,166,35,0.55)" : "#333c4a",
      "stroke-width": acc ? 1.8 : 1.3, ...(acc ? {} : { "stroke-dasharray": "3 4" }),
    }));
  });

  // nodes
  const state = c => c.role === "best" ? "best" : c.role === "seed" ? "seed" : c.accepted ? "frontier" : "rejected";
  const style = {
    seed:     { stroke: COL.blue,  fill: "#12203a", text: "#dbe6f5" },
    frontier: { stroke: COL.amber, fill: "rgba(245,166,35,0.11)", text: COL.amber },
    best:     { stroke: "#ffc94d", fill: "#ffc94d", text: "#111" },
    rejected: { stroke: "#3a4353", fill: "#141b26", text: "#8b95a6", dash: "3 3" },
  };
  cands.forEach(c => {
    const st = style[state(c)];
    const g = el("g", { class: "dag-node", "data-id": c.id, tabindex: 0, role: "button", "aria-label": `candidate ${c.id}, score ${c.score}` });
    const rect = el("rect", {
      x: X(c.id), y: Y(c.id), width: NW, height: NH, rx: 8,
      fill: st.fill, stroke: st.stroke, "stroke-width": 1.6,
      ...(st.dash ? { "stroke-dasharray": st.dash } : {}),
      ...(state(c) === "best" ? { filter: "url(#glow)" } : {}),
    });
    g.appendChild(rect);
    g.appendChild(el("text", { x: X(c.id) + 11, y: Y(c.id) + 15, fill: st.text, "font-family": "var(--font-mono)", "font-size": 10, opacity: 0.8, text: (c.role || `c${c.id}`) }));
    g.appendChild(el("text", { x: X(c.id) + 11, y: Y(c.id) + 28, fill: st.text, "font-family": "var(--font-mono)", "font-size": 14, "font-weight": 600, text: c.score.toFixed(2) }));
    g.addEventListener("click", () => { onSelect(c.id); markSelected(svg, c.id); });
    g.addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onSelect(c.id); markSelected(svg, c.id); } });
    svg.appendChild(g);
  });

  // glow filter
  const defs = el("defs");
  defs.innerHTML = `<filter id="glow" x="-40%" y="-40%" width="180%" height="180%"><feDropShadow dx="0" dy="0" stdDeviation="4" flood-color="#f5a623" flood-opacity="0.7"/></filter>`;
  svg.insertBefore(defs, svg.firstChild);

  mount.innerHTML = ""; mount.appendChild(svg);
  return svg;
}
function markSelected(svg, id) {
  $$(".dag-node", svg).forEach(n => n.classList.toggle("selected", n.getAttribute("data-id") == id));
}

/* ============================================================
   Evolution — candidate detail
   ============================================================ */
function jsonHighlight(obj) {
  const json = JSON.stringify(obj, Object.keys(obj).sort ? null : null, 2);
  return stableStringify(obj, 0);
}
function stableStringify(obj, ind) {
  const pad = "  ".repeat(ind), pad2 = "  ".repeat(ind + 1);
  if (obj === null) return `<span class="jn">null</span>`;
  if (typeof obj === "number") return `<span class="jn">${obj}</span>`;
  if (typeof obj === "string") return `<span class="js">"${obj}"</span>`;
  if (typeof obj === "boolean") return `<span class="jn">${obj}</span>`;
  const keys = Object.keys(obj);
  if (!keys.length) return "{}";
  const rows = keys.map(k => `${pad2}<span class="jk">"${k}"</span>: ${stableStringify(obj[k], ind + 1)}`);
  return `{\n${rows.join(",\n")}\n${pad}}`;
}
function renderDetail(mount, sub, c) {
  const stateChip = c.role === "best" ? ["best", "best"] : c.role === "seed" ? ["seed", "seed"] : c.accepted ? ["frontier", c.frontier ? "frontier" : "accepted"] : ["rejected", "rejected"];
  sub.textContent = c.parent == null ? "root" : `mutated from c${c.parent}`;
  const m = c.metrics || {};
  mount.innerHTML = `
    <div class="cd-head">
      <span class="cd-id">candidate c${c.id}</span>
      <span class="cd-chip ${stateChip[0]}">${stateChip[1]}</span>
      <span class="cd-chip ${c.accepted ? 'frontier' : 'rejected'}">score ${c.score.toFixed(3)}</span>
    </div>
    <div class="cd-note">${c.note || ""}</div>
    <div class="cd-metrics">
      <div class="cd-met"><div class="k">success</div><div class="v">${m.success_rate != null ? (m.success_rate * 100).toFixed(0) + "%" : "—"}</div></div>
      <div class="cd-met"><div class="k">attempts</div><div class="v">${m.attempts_per_payment ?? "—"}</div></div>
      <div class="cd-met"><div class="k">fee ppm</div><div class="v">${m.fee_ppm != null ? fmt(m.fee_ppm) : "—"}</div></div>
    </div>
    <div class="cd-json">${jsonHighlight(c.params)}</div>`;
}

/* ============================================================
   Evolution — unified text/JSON diff (line-based, LCS)
   ============================================================ */
function plainLines(obj) { return stablePlain(obj, 0).split("\n"); }
function stablePlain(obj, ind) {
  const pad = "  ".repeat(ind), pad2 = "  ".repeat(ind + 1);
  if (obj === null || typeof obj !== "object") return JSON.stringify(obj);
  const keys = Object.keys(obj);
  if (!keys.length) return "{}";
  const rows = keys.map(k => `${pad2}"${k}": ${stablePlain(obj[k], ind + 1)}`);
  return `{\n${rows.join(",\n")}\n${pad}}`;
}
function lcsDiff(a, b) {
  const n = a.length, m = b.length;
  const dp = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--)
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  const out = []; let i = 0, j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) { out.push({ t: "ctx", s: a[i] }); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) { out.push({ t: "del", s: a[i++] }); }
    else { out.push({ t: "add", s: b[j++] }); }
  }
  while (i < n) out.push({ t: "del", s: a[i++] });
  while (j < m) out.push({ t: "add", s: b[j++] });
  return out;
}
function renderDiff(view, aCand, bCand) {
  const rows = lcsDiff(plainLines(aCand.params), plainLines(bCand.params));
  const adds = rows.filter(r => r.t === "add").length, dels = rows.filter(r => r.t === "del").length;
  const esc = s => s.replace(/&/g, "&amp;").replace(/</g, "&lt;");

  // Collapse long runs of unchanged context so changes stand out. Keep CTX
  // lines around each change; fold the rest. Works for multi-line text too.
  const CTX = 2;
  const keep = new Array(rows.length).fill(false);
  rows.forEach((r, i) => {
    if (r.t !== "ctx") for (let j = Math.max(0, i - CTX); j <= Math.min(rows.length - 1, i + CTX); j++) keep[j] = true;
  });
  const hasChange = adds + dels > 0;
  if (!hasChange) keep.fill(true);

  const out = []; let folded = 0;
  const flush = () => { if (folded) { out.push(`<span class="dl fold">⋯ ${folded} unchanged line${folded > 1 ? "s" : ""}</span>`); folded = 0; } };
  rows.forEach((r, i) => {
    if (keep[i]) { flush(); out.push(`<span class="dl ${r.t}">${esc(r.s)}</span>`); }
    else folded++;
  });
  flush();

  const stat = hasChange
    ? `c${aCand.id} → c${bCand.id} &nbsp; <b class="a">+${adds}</b> / <b class="d">−${dels}</b> lines changed`
    : `c${aCand.id} → c${bCand.id} &nbsp; identical params`;
  view.innerHTML = `<div class="diff-stat">${stat}</div>${out.join("")}`;
}

/* ============================================================
   Evolution — wire up
   ============================================================ */
function initEvolution(run) {
  const cands = run.candidates || [];
  if (!cands.length) return;
  const byId = {}; cands.forEach(c => byId[c.id] = c);
  renderCompare($("#compare-grid"), run);

  const selA = $("#diff-a"), selB = $("#diff-b"), diffView = $("#diff-view");
  const opt = c => { const o = el("option", { value: c.id, text: `c${c.id} · ${c.role || (c.accepted ? "frontier" : "rejected")} · ${c.score.toFixed(2)}` }); return o; };
  cands.forEach(c => { selA.appendChild(opt(c)); selB.appendChild(opt(c)); });
  const best = cands.find(c => c.role === "best") || cands[cands.length - 1];
  const seed = cands.find(c => c.role === "seed") || cands[0];
  selA.value = seed.id; selB.value = best.id;
  const doDiff = () => renderDiff(diffView, byId[selA.value], byId[selB.value]);
  selA.addEventListener("change", doDiff);
  selB.addEventListener("change", doDiff);
  doDiff();

  const detail = $("#cand-detail"), detailSub = $("#detail-sub");
  const select = id => {
    const c = byId[id];
    renderDetail(detail, detailSub, c);
    // diff parent -> child for context
    if (c.parent != null) { selA.value = c.parent; selB.value = c.id; doDiff(); }
  };
  const svg = lineageDag($("#lineage-dag"), cands, select);
  // preselect best
  select(best.id); markSelected(svg, best.id);
}

/* ============================================================
   Boot
   ============================================================ */
async function boot() {
  initNav();

  // ---- Live run ----
  try {
    const run = await (await fetch("./data/run.json", { cache: "no-store" })).json();
    const s = run.stats;
    const delta = (run.best_score - run.seed_score).toFixed(2);
    $("#t-best").textContent = run.best_score.toFixed(2);
    $("#t-delta").textContent = "+" + delta;
    $("#t-evals").textContent = fmt(s.evals_done);
    $("#rf-lm").textContent = run.reflection_lm;
    $("#run-status").textContent = run.status === "running" ? "Run active" : run.status;

    const rt = $("#run-tiles");
    rt.appendChild(tile({ k: "Best score", v: run.best_score.toFixed(2), delta: `+${delta} vs seed ${run.seed_score.toFixed(2)}`, deltaClass: "up", hi: true }));
    rt.appendChild(tile({ k: "Success rate", v: (s.best_success_rate * 100).toFixed(0), unit: "%", delta: `+${((s.best_success_rate - s.seed_success_rate) * 100).toFixed(0)} pts`, deltaClass: "up" }));
    rt.appendChild(tile({ k: "Attempts / payment", v: s.best_attempts_per_payment.toFixed(1), delta: `${(s.best_attempts_per_payment - s.seed_attempts_per_payment).toFixed(1)} vs ${s.seed_attempts_per_payment}`, deltaClass: "down" }));
    rt.appendChild(tile({ k: "Fee (ppm on success)", v: fmt(s.best_fee_ppm), delta: `${s.best_fee_ppm - s.seed_fee_ppm} vs ${s.seed_fee_ppm}`, deltaClass: "down" }));

    scoreChart($("#score-chart"), run);
    paramsTable($("#params-table"), run);
    initEvolution(run);

    const upd = new Date(run.updated);
    $("#foot-updated").textContent = "run " + run.run_id + " · updated " + upd.toISOString().replace("T", " ").slice(0, 16) + "Z";
  } catch (e) {
    $("#run-tiles").innerHTML = `<div class="tile"><div class="tk">error</div><div class="tv" style="font-size:15px">run.json not loaded</div></div>`;
    console.error(e);
  }

  // ---- Corpus ----
  try {
    const c = await (await fetch("./data/corpus.json", { cache: "no-store" })).json();
    const ct = $("#corpus-tiles");
    ct.appendChild(tile({ k: "Scenario files", v: fmt(c.meta.files), delta: `${c.meta.splits.train} train · ${c.meta.splits.val} val · ${c.meta.splits.test} test`, deltaClass: "neutral", hi: true }));
    ct.appendChild(tile({ k: "Payment scenarios", v: fmt(c.meta.scenarios) }));
    ct.appendChild(tile({ k: "Topology types", v: c.topology.length, delta: c.topology.map(t => t.label).join(" · "), deltaClass: "neutral" }));
    ct.appendChild(tile({ k: "Bimodal share", v: Math.round(c.liquidity.find(l => l.label === "bimodal").value / c.meta.files * 100), unit: "%", delta: "hard regime", deltaClass: "neutral" }));

    $("#c-files-1").textContent = c.meta.files + " files";
    barChart($("#bars-topology"), c.topology, { color: [COL.amber, COL.blue, COL.aqua] });
    barChart($("#bars-liquidity"), c.liquidity, { color: [COL.orange, COL.blue] });
    barChart($("#bars-amount"), c.amount, { color: COL.amber });
    barChart($("#bars-parts"), c.max_parts, { color: COL.aqua });
  } catch (e) {
    $("#corpus-tiles").innerHTML = `<div class="tile"><div class="tk">error</div><div class="tv" style="font-size:15px">corpus.json not loaded</div></div>`;
    console.error(e);
  }
}

document.addEventListener("DOMContentLoaded", boot);
