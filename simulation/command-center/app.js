/* ==========================================================================
   lnd × GEPA — routing research report
   Vanilla JS. Hand-drawn SVG figures, no build step, no CDN JS.
   Every render is guarded on its mount existing, so both pages share this file.
   ========================================================================== */

const SVGNS = "http://www.w3.org/2000/svg";

/* Design tokens mirrored from style.css for SVG attribute use. */
const C = {
  ink:      "#1c1a16",
  ink2:     "#4e4941",
  ink3:     "#7a736a",
  ink4:     "#9c948a",
  rule:     "#ddd7c8",
  rule2:    "#cbc4b2",
  rule3:    "#b3ab97",
  grid:     "#e2ddd0",
  surface:  "#fbfaf6",
  paper:    "#f3f0e9",
  sunk:     "#ece8de",
  accent:   "#a83f22",
  series1:  "#2f6ea8",
  series2:  "#a83f22",
  neutral:  "#8a8175",
};

const $  = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];

const SVG_TAGS = new Set([
  "svg", "g", "path", "line", "circle", "rect", "text", "polyline", "polygon",
  "defs", "clipPath", "tspan", "marker", "pattern",
]);

const el = (tag, attrs = {}, kids = []) => {
  const n = SVG_TAGS.has(tag)
    ? document.createElementNS(SVGNS, tag)
    : document.createElement(tag);
  for (const k in attrs) {
    if (attrs[k] == null) continue;
    if (k === "text") n.textContent = attrs[k];
    else if (k === "html") n.innerHTML = attrs[k];
    else n.setAttribute(k, attrs[k]);
  }
  (Array.isArray(kids) ? kids : [kids]).forEach(c => c && n.appendChild(c));
  return n;
};

const fmt = n => Number(n).toLocaleString("en-US");
const esc = s => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;");

/* ==========================================================================
   Verified results, transcribed from simulation/lab/experiments/.
   Objective = success_rate − 0.01·min(extra_attempts,15) − 0.00002·min(fee_ppm,5000).
   ========================================================================== */

const ROUTERS = [
  {
    id: "lnd",
    name: "lnd production stack",
    note: "Dijkstra + mission control",
    mainnet: 0.694, hard: 0.309, ood: 0.357, combined: 0.333,
    mainSuccess: 0.790, mainAttempts: 19.8,
    attempts: 50, lines: null, evolved: false,
  },
  {
    id: "seed",
    name: "hand-written seed",
    note: "~300 lines, cheapest path + blacklist",
    mainnet: 0.762, hard: 0.530, ood: 0.487, combined: 0.509,
    mainSuccess: 0.820, mainAttempts: 6.1,
    attempts: 32, lines: 300, evolved: false,
  },
  {
    id: "hb1",
    name: "hb1",
    note: "evolved — hard-regime specialist",
    mainnet: 0.790, hard: 0.586, ood: 0.545, combined: 0.565,
    mainSuccess: 0.810, mainAttempts: 2.3,
    attempts: 9, lines: 872, evolved: true,
  },
  {
    id: "mx_c3",
    name: "mx_c3",
    note: "evolved — generalist, best overall",
    mainnet: 0.791, hard: 0.583, ood: 0.581, combined: 0.582,
    mainSuccess: 0.810, mainAttempts: 2.3,
    attempts: 9, lines: 1525, evolved: true,
  },
];

const MEASURES = [
  { key: "mainnet", label: "mainnet snapshot",     sub: "12,161 nodes · 100 payments" },
  { key: "hard",    label: "hard sealed test",     sub: "10 held-out bimodal scenarios" },
  { key: "ood",     label: "out-of-distribution",  sub: "corpus-v2 scale-free, never trained on" },
];

/* exp-011 — the three lineages, averaged over all three held-out tiers.
   Note this "combined" spans mainnet too, so it is not the synthetic-only
   combined column used in the scoreboard table. */
const LINEAGE = [
  { id: "lnd",   name: "lnd production stack", short: "lnd",   combined: 0.453,
    note: "baseline", evolved: false },
  { id: "seed",  name: "hand-written seed",    short: "seed",  combined: 0.593,
    note: "hand-written, ~300 lines", evolved: false },
  { id: "gen2",  name: "gen2",                 short: "gen2",  combined: 0.638,
    note: "lineage 3 — small seed + insights as prose", evolved: true },
  { id: "hb1",   name: "hb1",                  short: "hb1",   combined: 0.640,
    note: "lineage 1 — the breakthrough run", evolved: true },
  { id: "mx_c3", name: "mx_c3",                short: "mx_c3", combined: 0.652,
    note: "lineage 2 — continued from hb1 on a mixed corpus", evolved: true },
];

/* exp-008 — drift baseline, before any evolution on the drift corpus. */
const DRIFT = [
  {
    id: "lnd", name: "lnd production stack", short: "lnd",
    note: "its decay half-lives finally operate",
    val: 0.213, test: 0.203, valAtt: 59.5, testAtt: 34.5, evolved: false,
  },
  {
    id: "seed", name: "hand-written seed", short: "seed",
    note: "~300 lines, cheapest path + blacklist",
    val: 0.320, test: 0.377, valAtt: 38.2, testAtt: 48.3, evolved: false,
  },
  {
    id: "hb1", name: "hb1", short: "hb1",
    note: "evolved in a static world, no clock",
    val: 0.387, test: 0.455, valAtt: 8.6, testAtt: 11.8, evolved: true,
  },
  {
    id: "mx_c3", name: "mx_c3", short: "mx_c3",
    note: "evolved in a static world, no clock",
    val: 0.380, test: 0.457, valAtt: 11.3, testAtt: 12.3, evolved: true,
  },
  {
    id: "gen2", name: "gen2", short: "gen2",
    note: "evolved in a static world, no clock",
    val: 0.383, test: 0.456, valAtt: 8.9, testAtt: 12.7, evolved: true,
  },
];

const DRIFT_MEASURES = [
  { key: "val",  label: "drift validation",    sub: "8 files" },
  { key: "test", label: "drift held-out test", sub: "8 files" },
];

/* ==========================================================================
   Figure 1 — champions comparison
   Grouped horizontal bars: one group per held-out set, four routers per group.
   Color is emphasis only (evolved vs baseline); identity is on the labels, so
   no legend is needed for the routers themselves.
   ========================================================================== */

/* True when the mount is too narrow for full-length row labels, in which case
   the bar charts re-lay themselves out rather than scrolling sideways. */
const isNarrow = mount => mount.clientWidth > 0 && mount.clientWidth < 520;

function groupedBarPlot(mount, cfg) {
  if (!mount) return;
  const rows = cfg.rows, measures = cfg.measures;
  const narrow = isNarrow(mount);
  const W = narrow ? 360 : 780;
  const PAD = narrow
    ? { t: 8, r: 46, b: 42, l: 78 }
    : { t: 8, r: 54, b: 44, l: 158 };
  const BAR = narrow ? 13 : 15, GAP = 5;
  const GROUP_GAP = narrow ? 28 : 34, HEAD = narrow ? 18 : 20;

  const groupH = HEAD + rows.length * (BAR + GAP);
  const plotH = measures.length * groupH + (measures.length - 1) * (GROUP_GAP - GAP);
  const H = PAD.t + plotH + PAD.b;
  const iw = W - PAD.l - PAD.r;

  const xMax = cfg.xMax;
  const X = v => PAD.l + (v / xMax) * iw;

  const svg = el("svg", {
    viewBox: `0 0 ${W} ${H}`,
    role: "img",
    "aria-label": cfg.ariaLabel,
  });

  /* x grid — solid hairlines, one shade off the surface */
  const gridStep = narrow ? cfg.gridStepNarrow : cfg.gridStep;
  for (let v = 0; v <= xMax + 1e-9; v += gridStep) {
    const x = X(v);
    svg.appendChild(el("line", {
      x1: x, y1: PAD.t, x2: x, y2: PAD.t + plotH,
      stroke: v === 0 ? C.rule3 : C.grid, "stroke-width": 1,
    }));
    svg.appendChild(el("text", {
      x, y: H - PAD.b + 17, "text-anchor": "middle",
      class: "s-num", text: v.toFixed(cfg.tickDigits ?? 1),
    }));
  }
  svg.appendChild(el("text", {
    x: PAD.l + iw / 2, y: H - 2, "text-anchor": "middle",
    class: "s-label", text: "composite objective  →",
  }));

  const tip = el("div", { class: "tip" });

  let y = PAD.t;
  measures.forEach(m => {
    svg.appendChild(el("text", {
      x: 0, y: y + 12, class: "s-label-strong", text: m.label,
    }));
    svg.appendChild(el("line", {
      x1: 0, y1: y + 19, x2: W - PAD.r, y2: y + 19,
      stroke: C.rule, "stroke-width": 1,
    }));

    let by = y + HEAD;
    rows.forEach(r => {
      const v = r[m.key];
      const fill = r.evolved ? C.accent : C.neutral;

      svg.appendChild(el("text", {
        x: PAD.l - 10, y: by + BAR - 3.5, "text-anchor": "end",
        class: r.evolved ? "s-label-strong" : "s-label",
        text: narrow ? (r.short || r.id) : r.name,
      }));

      /* 4px rounded data-end, anchored to the zero baseline. */
      const w = Math.max(X(v) - PAD.l, 6);
      svg.appendChild(el("path", {
        d: `M ${PAD.l} ${by} H ${PAD.l + w - 4} a4 4 0 0 1 4 4 V ${by + BAR - 4}` +
           ` a4 4 0 0 1 -4 4 H ${PAD.l} Z`,
        fill,
      }));
      svg.appendChild(el("text", {
        x: PAD.l + w + 9, y: by + BAR - 3.5,
        class: "s-num", "font-size": 11,
        fill: r.evolved ? C.accent : C.ink3,
        text: v.toFixed(3),
      }));

      /* generous hit target for the hover layer */
      const hit = el("rect", {
        x: 0, y: by - 2, width: W, height: BAR + 4,
        fill: "transparent", style: "cursor:default",
      });
      hit.addEventListener("mouseenter", () => {
        const att = cfg.att(r, m);
        tip.classList.add("show");
        tip.innerHTML =
          `<div class="tt-h">${esc(r.name)}</div>` +
          `<div class="tt-r"><span class="l">${esc(m.label)}</span>` +
          `<span>${v.toFixed(3)}</span></div>` +
          `<div class="tt-r"><span class="l">attempts / payment</span>` +
          `<span>${att}</span></div>` +
          `<div class="tt-n">${esc(r.note)}</div>`;
        const rect = mount.getBoundingClientRect();
        const px = (X(v) / W) * rect.width;
        const py = ((by + BAR / 2) / H) * rect.height;
        tip.style.left =
          Math.min(Math.max(px + 16, 4), Math.max(rect.width - 210, 4)) + "px";
        tip.style.top = Math.max(py - 24, 2) + "px";
      });
      hit.addEventListener("mouseleave", () => tip.classList.remove("show"));
      svg.appendChild(hit);

      by += BAR + GAP;
    });
    y += groupH + (GROUP_GAP - GAP);
  });

  mount.innerHTML = "";
  mount.appendChild(svg);
  mount.appendChild(tip);
}

function championsPlot(mount) {
  groupedBarPlot(mount, {
    rows: ROUTERS,
    measures: MEASURES,
    xMax: 0.8,
    gridStep: 0.1,
    gridStepNarrow: 0.2,
    att: (r, m) => (m.key === "mainnet" ? r.mainAttempts : `≈${r.attempts}`),
    ariaLabel:
      "Composite objective by router on the mainnet snapshot, the hard sealed test " +
      "and out-of-distribution corpus-v2. The evolved routers hb1 and mx_c3 lead " +
      "on all three tiers.",
  });
}

/* exp-008 — the same idiom, one tier of difficulty down the y axis: every
   router scores lower once liquidity drifts, and the order barely moves. */
function driftPlot(mount) {
  groupedBarPlot(mount, {
    rows: DRIFT,
    measures: DRIFT_MEASURES,
    xMax: 0.5,
    gridStep: 0.1,
    gridStepNarrow: 0.25,
    tickDigits: 2,
    att: (r, m) => (m.key === "val" ? r.valAtt : r.testAtt).toFixed(1),
    ariaLabel:
      "Composite objective on the drift corpus before evolution. On the held-out " +
      "drift test lnd scores 0.203, the hand-written seed 0.377, and the three " +
      "evolved routers cluster near 0.456.",
  });
}

/* ==========================================================================
   Figure — three lineages, one band
   A dot plot on a single axis: the quantity of interest is how little space
   separates three independently bred routers, so the axis is zoomed and the
   band is drawn. Dots, not bars, because the axis does not start at zero.
   ========================================================================== */

function convergencePlot(mount) {
  if (!mount) return;
  const narrow = isNarrow(mount);
  const W = narrow ? 360 : 780;
  const PAD = narrow
    ? { t: 34, r: 44, b: 44, l: 78 }
    : { t: 34, r: 56, b: 46, l: 158 };
  const ROW = narrow ? 30 : 34;
  const plotH = LINEAGE.length * ROW;
  const H = PAD.t + plotH + PAD.b;
  const iw = W - PAD.l - PAD.r;

  const xMin = 0.40, xMax = 0.70;
  const X = v => PAD.l + ((v - xMin) / (xMax - xMin)) * iw;

  const band = LINEAGE.filter(r => r.evolved).map(r => r.combined);
  const bLo = Math.min(...band), bHi = Math.max(...band);

  const svg = el("svg", {
    viewBox: `0 0 ${W} ${H}`, role: "img",
    "aria-label":
      "Combined held-out objective across all three tiers. lnd scores 0.453 and " +
      "the hand-written seed 0.593, while the three evolved lineages gen2, hb1 " +
      "and mx_c3 fall inside a band from 0.638 to 0.652.",
  });

  /* the band first, so every rule and dot sits on top of it */
  svg.appendChild(el("rect", {
    x: X(bLo), y: PAD.t - 12, width: X(bHi) - X(bLo), height: plotH + 12,
    fill: C.accent, opacity: 0.09,
  }));
  svg.appendChild(el("path", {
    d: `M ${X(bLo)} ${PAD.t - 18} V ${PAD.t - 12} M ${X(bHi)} ${PAD.t - 18}` +
       ` V ${PAD.t - 12} M ${X(bLo)} ${PAD.t - 18} H ${X(bHi)}`,
    fill: "none", stroke: C.accent, "stroke-width": 1,
  }));
  svg.appendChild(el("text", {
    x: (X(bLo) + X(bHi)) / 2, y: PAD.t - 24, "text-anchor": "middle",
    class: "s-label-strong", fill: C.accent,
    text: narrow ? "0.014 apart" : "three lineages, 0.014 apart",
  }));

  const step = narrow ? 0.10 : 0.05;
  for (let v = xMin; v <= xMax + 1e-9; v += step) {
    const x = X(v);
    svg.appendChild(el("line", {
      x1: x, y1: PAD.t - 12, x2: x, y2: PAD.t + plotH,
      stroke: C.grid, "stroke-width": 1,
    }));
    svg.appendChild(el("text", {
      x, y: PAD.t + plotH + 18, "text-anchor": "middle",
      class: "s-num", text: v.toFixed(2),
    }));
  }
  svg.appendChild(el("text", {
    x: PAD.l + iw / 2, y: H - 4, "text-anchor": "middle", class: "s-label",
    text: narrow
      ? "combined objective  →"
      : "combined objective, all three held-out tiers  →",
  }));

  const tip = el("div", { class: "tip" });

  LINEAGE.forEach((r, i) => {
    const cy = PAD.t + i * ROW + ROW / 2;
    const color = r.evolved ? C.accent : C.neutral;

    svg.appendChild(el("text", {
      x: PAD.l - 12, y: cy + 4, "text-anchor": "end",
      class: r.evolved ? "s-label-strong" : "s-label",
      text: narrow ? (r.short || r.id) : r.name,
    }));
    /* Leader from the axis to the dot, dotted so it reads as a guide rather
       than as a bar measured from an origin the axis does not have. */
    svg.appendChild(el("line", {
      x1: PAD.l, y1: cy, x2: X(r.combined) - 7, y2: cy,
      stroke: C.rule2, "stroke-width": 1, "stroke-dasharray": "1 4",
    }));
    svg.appendChild(el("circle", {
      cx: X(r.combined), cy, r: r.evolved ? 5 : 4.2,
      fill: color, stroke: C.surface, "stroke-width": 1.6,
    }));
    svg.appendChild(el("text", {
      x: X(r.combined) + 12, y: cy + 4,
      class: "s-num", "font-size": 11,
      fill: r.evolved ? C.accent : C.ink3,
      text: r.combined.toFixed(3),
    }));

    const hit = el("rect", {
      x: 0, y: cy - ROW / 2, width: W, height: ROW,
      fill: "transparent", style: "cursor:default",
    });
    hit.addEventListener("mouseenter", () => {
      tip.classList.add("show");
      tip.innerHTML =
        `<div class="tt-h">${esc(r.name)}</div>` +
        `<div class="tt-r"><span class="l">combined objective</span>` +
        `<span>${r.combined.toFixed(3)}</span></div>` +
        `<div class="tt-n">${esc(r.note)}</div>`;
      const rect = mount.getBoundingClientRect();
      tip.style.left =
        Math.min(Math.max((X(r.combined) / W) * rect.width + 16, 4),
          Math.max(rect.width - 210, 4)) + "px";
      tip.style.top = Math.max((cy / H) * rect.height - 24, 2) + "px";
    });
    hit.addEventListener("mouseleave", () => tip.classList.remove("show"));
    svg.appendChild(hit);
  });

  mount.innerHTML = "";
  mount.appendChild(svg);
  mount.appendChild(tip);
}

/* ==========================================================================
   Figure — attempts per payment on the mainnet graph
   One measure, three entities: a plain bar chart, single hue, emphasis on the
   evolved pair. The story is the ratio, so the ratio is direct-labelled.
   ========================================================================== */

function attemptsPlot(mount) {
  if (!mount) return;
  const narrow = isNarrow(mount);
  const rows = [
    {
      name: narrow ? "lnd" : "lnd production stack",
      sub: "Dijkstra + mission control",
      v: 19.8, evolved: false,
    },
    {
      name: narrow ? "seed" : "hand-written seed",
      sub: "~300 lines, hand-written",
      v: 6.1, evolved: false,
    },
    {
      name: narrow ? "hb1, mx_c3" : "hb1 and mx_c3",
      sub: "evolved, both at 2.3",
      v: 2.3, evolved: true,
    },
  ];
  const W = narrow ? 360 : 780;
  const BAR = narrow ? 20 : 24, GAP = narrow ? 26 : 24;
  const PAD = narrow
    ? { t: 6, r: 52, b: 44, l: 92 }
    : { t: 6, r: 118, b: 44, l: 188 };
  const plotH = rows.length * BAR + (rows.length - 1) * GAP;
  const H = PAD.t + plotH + PAD.b;
  const iw = W - PAD.l - PAD.r;
  const xMax = 20;
  const X = v => PAD.l + (v / xMax) * iw;

  const svg = el("svg", {
    viewBox: `0 0 ${W} ${H}`, role: "img",
    "aria-label":
      "Attempts per payment on the mainnet snapshot: lnd 19.8, hand-written seed " +
      "6.1, evolved routers 2.3.",
  });

  for (let v = 0; v <= xMax; v += (narrow ? 10 : 5)) {
    const x = X(v);
    svg.appendChild(el("line", {
      x1: x, y1: PAD.t, x2: x, y2: PAD.t + plotH,
      stroke: v === 0 ? C.rule3 : C.grid, "stroke-width": 1,
    }));
    svg.appendChild(el("text", {
      x, y: PAD.t + plotH + 18, "text-anchor": "middle",
      class: "s-num", text: v,
    }));
  }
  svg.appendChild(el("text", {
    x: PAD.l, y: H - 2, class: "s-label",
    text: "HTLC attempts per payment  →",
  }));

  let y = PAD.t;
  rows.forEach(r => {
    const fill = r.evolved ? C.accent : C.neutral;
    svg.appendChild(el("text", {
      x: PAD.l - 12, y: y + 15, "text-anchor": "end",
      class: r.evolved ? "s-label-strong" : "s-label", text: r.name,
    }));
    if (!narrow) {
      svg.appendChild(el("text", {
        x: PAD.l - 12, y: y + 29, "text-anchor": "end",
        class: "s-num", text: r.sub,
      }));
    }
    const w = Math.max(X(r.v) - PAD.l, 8);
    svg.appendChild(el("path", {
      d: `M ${PAD.l} ${y} H ${PAD.l + w - 4} a4 4 0 0 1 4 4 V ${y + BAR - 4}` +
         ` a4 4 0 0 1 -4 4 H ${PAD.l} Z`,
      fill,
    }));
    svg.appendChild(el("text", {
      x: PAD.l + w + 10, y: y + BAR / 2 + (narrow ? 4 : 5),
      "font-family": "var(--mono)", "font-size": narrow ? 12 : 14,
      "font-weight": 500,
      fill: r.evolved ? C.accent : C.ink2, text: r.v.toFixed(1),
    }));
    y += BAR + GAP;
  });

  /* the ratio, drawn as a measured span between the two ends */
  if (!narrow) {
    const yTop = PAD.t + BAR / 2, yBot = PAD.t + 2 * (BAR + GAP) + BAR / 2;
    const bx = W - PAD.r + 56;
    svg.appendChild(el("path", {
      d: `M ${bx - 6} ${yTop} H ${bx} V ${yBot} H ${bx - 6}`,
      fill: "none", stroke: C.rule3, "stroke-width": 1,
    }));
    svg.appendChild(el("text", {
      x: bx + 6, y: (yTop + yBot) / 2 - 2,
      "font-family": "var(--mono)", "font-size": 15, "font-weight": 500,
      fill: C.accent, text: "8.6×",
    }));
    svg.appendChild(el("text", {
      x: bx + 6, y: (yTop + yBot) / 2 + 14, class: "s-num", text: "fewer",
    }));
  }

  mount.innerHTML = "";
  mount.appendChild(svg);
}

/* ==========================================================================
   Figure 2 — the rediscovered bimodal prior
   P(success) as an explicit function of amount / capacity: a decaying
   exponential "low mode" plus a logistic cliff near capacity.
   ========================================================================== */

const priorParts = r => {
  const low = 0.45 * Math.exp(-r / 0.025);
  const high = 0.50 / (1 + Math.exp((r - 0.92) / 0.04));
  let p = 0.025 + low + high;
  if (p > 0.985) p = 0.985;
  if (p < 0.005) p = 0.005;
  return { low, high, p };
};

function priorCurve(mount) {
  if (!mount) return;
  const W = 780, H = 330;
  const PAD = { t: 16, r: 122, b: 46, l: 64 };
  const iw = W - PAD.l - PAD.r, ih = H - PAD.t - PAD.b;

  const X = r => PAD.l + r * iw;
  const Y = p => PAD.t + (1 - p) * ih;

  const svg = el("svg", {
    viewBox: `0 0 ${W} ${H}`, role: "img",
    "aria-label":
      "Success probability against amount over capacity. The evolved prior starts " +
      "near 0.98 for tiny amounts, decays to about 0.53 by 10% of capacity, holds " +
      "flat, then falls off a logistic cliff at 92% of capacity.",
  });

  /* grid + axes */
  for (let p = 0; p <= 1.0001; p += 0.25) {
    const y = Y(p);
    svg.appendChild(el("line", {
      x1: PAD.l, y1: y, x2: PAD.l + iw, y2: y,
      stroke: p === 0 ? C.rule3 : C.grid, "stroke-width": 1,
    }));
    svg.appendChild(el("text", {
      x: PAD.l - 9, y: y + 3.5, "text-anchor": "end",
      class: "s-num", text: p.toFixed(2),
    }));
  }
  for (let r = 0; r <= 1.0001; r += 0.25) {
    svg.appendChild(el("text", {
      x: X(r), y: H - PAD.b + 18, "text-anchor": "middle",
      class: "s-num", text: r.toFixed(2),
    }));
  }
  svg.appendChild(el("text", {
    x: PAD.l + iw / 2, y: H - 8, "text-anchor": "middle",
    class: "s-label", text: "amount ÷ channel capacity",
  }));
  svg.appendChild(el("text", {
    x: 15, y: PAD.t + ih / 2, class: "s-label",
    transform: `rotate(-90 15 ${PAD.t + ih / 2})`,
    "text-anchor": "middle", text: "P(success)",
  }));

  const path = (f, from = 0, to = 1, N = 400) => {
    const pts = [];
    for (let i = 0; i <= N; i++) {
      const r = from + (to - from) * (i / N);
      pts.push(`${X(r).toFixed(2)},${Y(f(r)).toFixed(2)}`);
    }
    return pts.join(" ");
  };

  /* The low mode only says anything over the first fifth of the range, so it is
     only drawn there — a dashed line hugging the axis would read as data. */
  svg.appendChild(el("polyline", {
    points: path(r => priorParts(r).low, 0, 0.13), fill: "none",
    stroke: C.ink4, "stroke-width": 1.4, "stroke-dasharray": "4 4",
  }));

  /* the prior itself */
  svg.appendChild(el("polyline", {
    points: path(r => priorParts(r).p), fill: "none",
    stroke: C.accent, "stroke-width": 2, "stroke-linejoin": "round",
  }));

  /* Where the logistic cliff sits, marked once. */
  svg.appendChild(el("line", {
    x1: X(0.92), y1: PAD.t, x2: X(0.92), y2: Y(0), stroke: C.rule2, "stroke-width": 1,
  }));
  svg.appendChild(el("text", {
    x: X(0.92), y: PAD.t - 4, "text-anchor": "middle", class: "s-num",
    text: "r = 0.92",
  }));

  /* selective direct labels on the features that carry the story */
  const ann = (r, dx, dy, lines, anchor = "start") => {
    const p = priorParts(r).p;
    svg.appendChild(el("circle", {
      cx: X(r), cy: Y(p), r: 3.4, fill: C.accent,
      stroke: C.surface, "stroke-width": 2,
    }));
    lines.forEach((t, i) => svg.appendChild(el("text", {
      x: X(r) + dx, y: Y(p) + dy + i * 13, "text-anchor": anchor,
      class: i === 0 ? "s-label-strong" : "s-label", text: t,
    })));
  };
  ann(0.008, 14, -16, ["tiny amounts nearly always pass", "0.45 · e^(−r / 0.025)"]);
  ann(0.45, 0, -18, ["flat middle ≈ 0.53", "the logistic mode carries it"], "middle");
  ann(0.965, -14, 10, ["then a cliff", "0.50 / (1 + e^((r−0.92) / 0.04))"], "end");

  mount.innerHTML = "";
  mount.appendChild(svg);
}

/* ==========================================================================
   Figure — score over iterations (live run)
   ========================================================================== */

function scoreChart(mount, run) {
  if (!mount) return;
  const its = run.iterations || [];
  if (its.length < 2) {
    mount.innerHTML =
      `<p class="s-empty" style="font-family:var(--mono);font-size:11.5px;` +
      `color:var(--ink-3);margin:0">Only ${its.length} iteration exported so ` +
      `far — the curve appears once the run has more than one.</p>`;
    return;
  }

  const W = 720, H = 300, PAD = { t: 16, r: 46, b: 38, l: 44 };
  const iw = W - PAD.l - PAD.r, ih = H - PAD.t - PAD.b;
  const xs = its.map(d => d.i);
  const xMin = Math.min(...xs), xMax = Math.max(...xs);
  const all = its.flatMap(d => [d.candidate_score, d.best_score]).concat(run.seed_score);
  const step = 0.05;
  let yMin = Math.max(0, Math.floor((Math.min(...all) - 0.01) / step) * step);
  const yMax = Math.ceil((Math.max(...all) + 0.01) / step) * step;

  const X = i => PAD.l + (xMax === xMin ? 0.5 : (i - xMin) / (xMax - xMin)) * iw;
  const Y = v => PAD.t + (1 - (v - yMin) / (yMax - yMin)) * ih;

  const svg = el("svg", {
    viewBox: `0 0 ${W} ${H}`, role: "img",
    "aria-label": "Best and proposed candidate score over GEPA iterations.",
  });

  const ticks = Math.round((yMax - yMin) / step);
  for (let i = 0; i <= ticks; i++) {
    const v = yMin + step * i, y = Y(v);
    svg.appendChild(el("line", {
      x1: PAD.l, y1: y, x2: W - PAD.r, y2: y,
      stroke: C.grid, "stroke-width": 1,
    }));
    svg.appendChild(el("text", {
      x: PAD.l - 9, y: y + 3.5, "text-anchor": "end",
      class: "s-num", text: v.toFixed(2),
    }));
  }
  its.forEach((d, i) => {
    if (i % Math.ceil(its.length / 10) !== 0 && i !== its.length - 1) return;
    svg.appendChild(el("text", {
      x: X(d.i), y: H - PAD.b + 18, "text-anchor": "middle",
      class: "s-num", text: d.i,
    }));
  });
  svg.appendChild(el("text", {
    x: PAD.l + iw / 2, y: H - 4, "text-anchor": "middle",
    class: "s-label", text: "iteration",
  }));

  const by = Y(run.seed_score);
  svg.appendChild(el("line", {
    x1: PAD.l, y1: by, x2: W - PAD.r, y2: by,
    stroke: C.ink4, "stroke-width": 1.4, "stroke-dasharray": "5 4",
  }));
  svg.appendChild(el("text", {
    x: W - PAD.r + 6, y: by + 3.5, class: "s-num", text: "seed",
  }));

  const line = (acc, color, width) => svg.appendChild(el("polyline", {
    points: its.map(d => `${X(d.i)},${Y(acc(d))}`).join(" "),
    fill: "none", stroke: color, "stroke-width": width,
    "stroke-linejoin": "round", "stroke-linecap": "round",
  }));

  line(d => d.candidate_score, C.series1, 1.6);
  its.forEach(d => svg.appendChild(el("circle", {
    cx: X(d.i), cy: Y(d.candidate_score), r: 3.2,
    fill: C.series1, stroke: C.surface, "stroke-width": 2,
  })));
  line(d => d.best_score, C.accent, 2);
  its.forEach(d => svg.appendChild(el("circle", {
    cx: X(d.i), cy: Y(d.best_score), r: 4,
    fill: C.accent, stroke: C.surface, "stroke-width": 2,
  })));

  const last = its[its.length - 1];
  svg.appendChild(el("text", {
    x: X(last.i) - 6, y: Y(last.best_score) - 12, "text-anchor": "end",
    class: "s-label-strong", fill: C.accent, text: last.best_score.toFixed(3),
  }));

  const cross = el("line", {
    x1: 0, y1: PAD.t, x2: 0, y2: H - PAD.b,
    stroke: C.rule3, "stroke-width": 1, opacity: 0,
  });
  svg.appendChild(cross);
  mount.innerHTML = "";
  mount.appendChild(svg);
  const tip = el("div", { class: "tip" });
  mount.appendChild(tip);

  const hit = el("rect", {
    x: PAD.l, y: PAD.t, width: iw, height: ih,
    fill: "transparent", style: "cursor:crosshair",
  });
  svg.appendChild(hit);
  hit.addEventListener("mousemove", evt => {
    const pt = svg.createSVGPoint();
    pt.x = evt.clientX; pt.y = evt.clientY;
    const p = pt.matrixTransform(svg.getScreenCTM().inverse());
    let near = its[0], nd = Infinity;
    its.forEach(d => {
      const dx = Math.abs(X(d.i) - p.x);
      if (dx < nd) { nd = dx; near = d; }
    });
    const cx = X(near.i);
    cross.setAttribute("x1", cx); cross.setAttribute("x2", cx);
    cross.setAttribute("opacity", 1);
    tip.classList.add("show");
    tip.innerHTML =
      `<div class="tt-h">iteration ${near.i}</div>` +
      `<div class="tt-r"><span class="l"><i style="background:${C.accent}"></i>` +
      `best</span><span>${near.best_score.toFixed(3)}</span></div>` +
      `<div class="tt-r"><span class="l"><i style="background:${C.series1}"></i>` +
      `proposed</span><span>${near.candidate_score.toFixed(3)}</span></div>` +
      `<div class="tt-n">${esc(near.note || "")}</div>`;
    const rect = mount.getBoundingClientRect();
    const px = (cx / W) * rect.width;
    tip.style.left =
      Math.min(Math.max(px + 14, 4), Math.max(rect.width - 190, 4)) + "px";
    tip.style.top = Math.max((Y(near.best_score) / H) * rect.height - 14, 2) + "px";
  });
  hit.addEventListener("mouseleave", () => {
    cross.setAttribute("opacity", 0);
    tip.classList.remove("show");
  });
}

/* ==========================================================================
   Category bars (corpus) — single hue, labels carry identity
   ========================================================================== */

function barChart(mount, data) {
  if (!mount) return;
  mount.innerHTML = "";
  const max = Math.max(...data.map(d => d.value));
  const sum = data.reduce((a, d) => a + d.value, 0);
  data.forEach(d => {
    const row = el("div", { class: "barrow" });
    row.appendChild(el("div", { class: "bl", text: d.label }));
    const track = el("div", { class: "bt" });
    const fill = el("div", { class: "bf" });
    fill.style.width = "0%";
    track.appendChild(fill);
    row.appendChild(track);
    row.appendChild(el("div", {
      class: "bv",
      html: `${fmt(d.value)} <small>${((d.value / sum) * 100).toFixed(0)}%</small>`,
    }));
    mount.appendChild(row);
    requestAnimationFrame(() => {
      fill.style.width = (d.value / max * 100) + "%";
    });
  });
}

/* ==========================================================================
   Lineage DAG
   ========================================================================== */

function lineageDag(mount, cands, onSelect) {
  if (!mount) return null;
  const kids = {};
  cands.forEach(c => {
    if (c.parent != null) (kids[c.parent] ??= []).push(c.id);
  });
  Object.values(kids).forEach(a => a.sort((x, y) => x - y));
  const roots = cands.filter(c => c.parent == null).map(c => c.id);

  const depth = {};
  const setDepth = (id, d) => {
    depth[id] = d;
    (kids[id] || []).forEach(k => setDepth(k, d + 1));
  };
  roots.forEach(r => setDepth(r, 0));

  const y = {};
  let leaf = 0;
  const assignY = id => {
    const ch = kids[id] || [];
    if (!ch.length) { y[id] = leaf++; return; }
    ch.forEach(assignY);
    y[id] = (y[ch[0]] + y[ch[ch.length - 1]]) / 2;
  };
  roots.forEach(assignY);

  const NW = 112, NH = 40, xGap = 152, yGap = 54, mx = 2, my = 8;
  const maxD = Math.max(...Object.values(depth));
  const W = mx * 2 + maxD * xGap + NW;
  const H = my * 2 + (leaf - 1) * yGap + NH;
  const X = id => mx + depth[id] * xGap;
  const Y = id => my + y[id] * yGap;

  const svg = el("svg", {
    viewBox: `0 0 ${W} ${H}`, width: W, height: H,
    style: `min-width:${W}px`, role: "img",
    "aria-label": "Candidate lineage: parent to child mutations.",
  });

  cands.forEach(c => {
    if (c.parent == null || depth[c.parent] == null) return;
    const x1 = X(c.parent) + NW, y1 = Y(c.parent) + NH / 2;
    const x2 = X(c.id), y2 = Y(c.id) + NH / 2;
    const mid = (x1 + x2) / 2;
    svg.appendChild(el("path", {
      d: `M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`,
      fill: "none", stroke: c.accepted ? C.rule3 : C.rule,
      "stroke-width": c.accepted ? 1.5 : 1,
    }));
  });

  const state = c =>
    c.role === "best" ? "best" :
    c.role === "seed" ? "seed" :
    c.accepted ? "kept" : "rejected";
  const style = {
    seed:     { stroke: C.rule3, fill: C.surface, text: C.ink2, label: C.ink3 },
    kept:     { stroke: C.rule3, fill: C.surface, text: C.ink,  label: C.ink3 },
    best:     { stroke: C.accent, fill: "rgba(168,63,34,0.08)", text: C.accent, label: C.accent },
    rejected: { stroke: C.rule, fill: "transparent", text: C.ink4, label: C.ink4 },
  };

  cands.forEach(c => {
    const st = style[state(c)];
    const g = el("g", {
      class: "dag-node", "data-id": c.id, tabindex: 0, role: "button",
      "aria-label": `candidate ${c.id}, score ${c.score}`,
    });
    g.appendChild(el("rect", {
      x: X(c.id), y: Y(c.id), width: NW, height: NH, rx: 2,
      fill: st.fill, stroke: st.stroke,
      "stroke-width": state(c) === "best" ? 1.6 : 1,
    }));
    g.appendChild(el("text", {
      x: X(c.id) + 11, y: Y(c.id) + 16, fill: st.label,
      class: "s-num", text: c.role || (c.accepted ? "kept" : "rejected"),
    }));
    g.appendChild(el("text", {
      x: X(c.id) + 11, y: Y(c.id) + 31, fill: st.text,
      "font-family": "var(--mono)", "font-size": 14, "font-weight": 500,
      text: c.score.toFixed(3),
    }));
    const pick = () => { onSelect(c.id); markSelected(svg, c.id); };
    g.addEventListener("click", pick);
    g.addEventListener("keydown", e => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); pick(); }
    });
    svg.appendChild(g);
  });

  mount.innerHTML = "";
  mount.appendChild(svg);
  return svg;
}

function markSelected(svg, id) {
  $$(".dag-node", svg).forEach(n =>
    n.classList.toggle("selected", n.getAttribute("data-id") == id));
}

/* ==========================================================================
   Candidate detail — code-mode aware
   ========================================================================== */

const isCode = c => c && c.params && typeof c.params.source === "string";

function candidateLines(c) {
  if (isCode(c)) return c.params.source.split("\n");
  return stablePlain(c.params ?? {}, 0).split("\n");
}

function stablePlain(obj, ind) {
  const pad = "  ".repeat(ind), pad2 = "  ".repeat(ind + 1);
  if (obj === null || typeof obj !== "object") return JSON.stringify(obj);
  const keys = Object.keys(obj);
  if (!keys.length) return "{}";
  return `{\n${keys.map(k => `${pad2}"${k}": ${stablePlain(obj[k], ind + 1)}`)
    .join(",\n")}\n${pad}}`;
}

/* Minimal Go highlighting: comments, keywords, strings. */
const GO_KW = new RegExp(
  "\\b(func|return|if|else|for|range|var|const|type|struct|interface|map|" +
  "package|import|switch|case|default|break|continue|go|defer|nil|true|false)\\b",
  "g");

function highlightGo(src) {
  return esc(src)
    .replace(/(&quot;|")((?:[^"\\\n]|\\.)*)\1/g, m => `<span class="st">${m}</span>`)
    .replace(GO_KW, m => `<span class="kw">${m}</span>`)
    .replace(/(\/\/[^\n]*)/g, m => `<span class="cm">${m}</span>`);
}

function renderDetail(mount, sub, c) {
  if (!mount) return;
  const lines = candidateLines(c);
  const role = c.role || (c.accepted ? "kept on the frontier" : "rejected");
  if (sub) {
    sub.textContent = c.parent == null
      ? "root of the lineage"
      : `mutated from candidate ${c.parent}`;
  }

  /* Show the head of the file: the contract comment plus the first symbols. */
  const head = lines.slice(0, 60).join("\n");
  const body = isCode(c)
    ? `<pre class="cd-src">${highlightGo(head)}</pre>`
    : `<pre class="cd-src">${esc(lines.join("\n"))}</pre>`;

  mount.innerHTML =
    `<div class="cd-top">` +
      `<span class="id">candidate ${c.id}</span>` +
      `<span class="role">${esc(role)}</span>` +
      `<span>score ${c.score.toFixed(3)}</span>` +
      (isCode(c) ? `<span>${fmt(lines.length)} lines of Go</span>` : "") +
    `</div>` +
    `<div class="cd-note">${esc(c.note || (isCode(c)
      ? "The whole routing algorithm is the candidate: GEPA rewrites this file " +
        "and the evaluator compiles it into the simulator via a build overlay."
      : ""))}</div>` +
    body;
}

/* ==========================================================================
   Diff viewer (line-based LCS)
   ========================================================================== */

function lcsDiff(a, b) {
  /* Guard the quadratic table: code-mode candidates run to 1500+ lines. */
  const LIMIT = 1200;
  if (a.length > LIMIT || b.length > LIMIT) {
    const out = [];
    const common = new Set(b);
    a.forEach(s => out.push({ t: common.has(s) ? "ctx" : "del", s }));
    const seen = new Set(a);
    b.forEach(s => { if (!seen.has(s)) out.push({ t: "add", s }); });
    return out;
  }
  const n = a.length, m = b.length;
  const dp = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j]
        ? dp[i + 1][j + 1] + 1
        : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const out = [];
  let i = 0, j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) { out.push({ t: "ctx", s: a[i] }); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) out.push({ t: "del", s: a[i++] });
    else out.push({ t: "add", s: b[j++] });
  }
  while (i < n) out.push({ t: "del", s: a[i++] });
  while (j < m) out.push({ t: "add", s: b[j++] });
  return out;
}

function renderDiff(view, aCand, bCand) {
  if (!view) return;
  const rows = lcsDiff(candidateLines(aCand), candidateLines(bCand));
  const adds = rows.filter(r => r.t === "add").length;
  const dels = rows.filter(r => r.t === "del").length;

  const CTX = 2;
  const keep = new Array(rows.length).fill(false);
  rows.forEach((r, i) => {
    if (r.t !== "ctx") {
      for (let j = Math.max(0, i - CTX); j <= Math.min(rows.length - 1, i + CTX); j++) {
        keep[j] = true;
      }
    }
  });
  if (adds + dels === 0) keep.fill(true);

  const out = [];
  let folded = 0;
  const flush = () => {
    if (folded) {
      out.push(`<span class="dl fold">⋯ ${fmt(folded)} unchanged ` +
        `line${folded > 1 ? "s" : ""}</span>`);
      folded = 0;
    }
  };
  rows.forEach((r, i) => {
    if (keep[i]) { flush(); out.push(`<span class="dl ${r.t}">${esc(r.s)}</span>`); }
    else folded++;
  });
  flush();

  const stat = adds + dels
    ? `candidate ${aCand.id} → ${bCand.id} &nbsp; <b class="a">+${fmt(adds)}</b> / ` +
      `<b class="d">−${fmt(dels)}</b> lines`
    : `candidate ${aCand.id} → ${bCand.id} &nbsp; identical`;
  view.innerHTML = `<div class="diff-stat">${stat}</div>${out.join("")}`;
}

/* ==========================================================================
   Live-run wiring
   ========================================================================== */

function initLineage(run) {
  const cands = (run.candidates || []).filter(c => c && typeof c.score === "number");
  if (!cands.length) return;
  const byId = {};
  cands.forEach(c => byId[c.id] = c);

  const selA = $("#diff-a"), selB = $("#diff-b"), diffView = $("#diff-view");
  const best = cands.find(c => c.role === "best") || cands[cands.length - 1];
  const seed = cands.find(c => c.role === "seed") || cands[0];

  if (selA && selB) {
    cands.forEach(c => {
      const label = `${c.id} · ${c.role || (c.accepted ? "kept" : "rejected")} · ` +
        c.score.toFixed(3);
      selA.appendChild(el("option", { value: c.id, text: label }));
      selB.appendChild(el("option", { value: c.id, text: label }));
    });
    selA.value = seed.id;
    selB.value = best.id;
    const doDiff = () => renderDiff(diffView, byId[selA.value], byId[selB.value]);
    selA.addEventListener("change", doDiff);
    selB.addEventListener("change", doDiff);
    doDiff();
  }

  const detail = $("#cand-detail"), detailSub = $("#detail-sub");
  const select = id => {
    const c = byId[id];
    renderDetail(detail, detailSub, c);
    if (c.parent != null && selA && selB && byId[c.parent]) {
      selA.value = c.parent;
      selB.value = c.id;
      renderDiff(diffView, byId[c.parent], c);
    }
  };
  const svg = lineageDag($("#lineage-dag"), cands, select);
  select(best.id);
  if (svg) markSelected(svg, best.id);
}

function statRow(mount, run) {
  if (!mount) return;
  const s = run.stats || {};
  const has = v => typeof v === "number" && isFinite(v);
  const items = [
    {
      k: "seed score",
      v: run.seed_score.toFixed(3),
      d: "the in-tree seed router",
    },
    {
      k: "best accepted",
      v: (run.iterations || []).reduce((m, i) => Math.max(m, i.best_score),
        run.seed_score).toFixed(3),
      d: "held on the frontier",
    },
    {
      k: "evals spent",
      v: has(s.evals_done) ? fmt(s.evals_done) : "—",
      d: "of a 400-eval budget",
    },
    {
      k: "distinct candidates",
      v: has(s.distinct_candidates) ? fmt(s.distinct_candidates) : "—",
      d: "compiled and scored",
    },
  ];
  mount.innerHTML = items.map(i =>
    `<div class="s"><div class="k">${i.k}</div>` +
    `<div class="v">${i.v}</div><div class="d">${i.d}</div></div>`).join("");
}

/* ==========================================================================
   Boot
   ========================================================================== */

async function boot() {
  /* static figures first — they never depend on the network */
  const drawStatic = () => {
    championsPlot($("#fig-champions"));
    attemptsPlot($("#fig-attempts"));
    priorCurve($("#fig-prior"));
    convergencePlot($("#fig-convergence"));
    driftPlot($("#fig-drift"));
  };
  drawStatic();

  /* The two bar charts pick a narrow layout below 520px, so they are redrawn
     when the breakpoint is actually crossed — not on every resize tick. */
  let wasNarrow = window.innerWidth < 560;
  let t = null;
  window.addEventListener("resize", () => {
    clearTimeout(t);
    t = setTimeout(() => {
      const now = window.innerWidth < 560;
      if (now !== wasNarrow) { wasNarrow = now; drawStatic(); }
    }, 160);
  });

  const needsRun = $("#score-chart") || $("#run-stats") || $("#lineage-dag");
  if (needsRun) {
    try {
      const run = await (await fetch("./data/run.json", { cache: "no-store" })).json();

      const set = (sel, text) => { const n = $(sel); if (n) n.textContent = text; };
      set("#rb-run", run.run_id);
      set("#rb-lm", run.reflection_lm);
      set("#rb-mode", run.mode);
      set("#rb-status", run.status);
      set("#foot-run", `run ${run.run_id} · ${run.stats?.evals_done ?? "?"} evals`);

      statRow($("#run-stats"), run);
      scoreChart($("#score-chart"), run);
      initLineage(run);
    } catch (e) {
      const n = $("#run-stats");
      if (n) {
        n.innerHTML =
          `<div class="s"><div class="k">run data</div>` +
          `<div class="v" style="font-size:1.05rem">unavailable</div>` +
          `<div class="d">data/run.json did not load</div></div>`;
      }
      console.error(e);
    }
  }

  if ($("#bars-topology")) {
    try {
      const c = await (await fetch("./data/corpus.json", { cache: "no-store" })).json();
      const set = (sel, text) => { const n = $(sel); if (n) n.textContent = text; };
      set("#c-files", fmt(c.meta.files));
      set("#c-scen", fmt(c.meta.scenarios));
      set("#c-split",
        `${c.meta.splits.train} train · ${c.meta.splits.val} val · ` +
        `${c.meta.splits.test} test`);
      barChart($("#bars-topology"), c.topology);
      barChart($("#bars-liquidity"), c.liquidity);
      barChart($("#bars-amount"), c.amount);
      barChart($("#bars-parts"), c.max_parts);
    } catch (e) {
      console.error(e);
    }
  }
}

document.addEventListener("DOMContentLoaded", boot);
