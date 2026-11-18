// VoxKube docs site: live manifest (index.html), API explorer (api.html), README renderer (docs.html).
// The API data comes from data/crds.json, generated from the CRDs by hack/gen-site.py.

const REPO = "https://github.com/yboikov/voxkube-operator";
const SKIP_TOP = new Set(["apiVersion", "kind", "metadata"]);

// ---------- shared ----------

let dataPromise;
function loadData() {
  dataPromise ??= fetch("data/crds.json").then((r) => {
    if (!r.ok) throw new Error(`data/crds.json: HTTP ${r.status}`);
    return r.json();
  });
  return dataPromise;
}

const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

// Inline markup for CRD descriptions: escape, then `code`.
const inline = (s) => esc(s).replace(/`([^`]+)`/g, "<code>$1</code>");

function paragraphs(desc) {
  if (!desc) return [];
  return desc.split(/\n\s*\n/).map((p) => p.replace(/\s*\n\s*/g, " ").trim()).filter(Boolean);
}
const firstSentence = (desc) => {
  const p = paragraphs(desc)[0] || "";
  const m = p.match(/^(.+?[.!?])(\s|$)/);
  return m ? m[1] : p;
};

function children(s) {
  if (!s) return null;
  if (s.properties) return { props: s.properties, required: s.required || [] };
  if (s.type === "array" && s.items) return children(s.items);
  if (s.additionalProperties && typeof s.additionalProperties === "object" && s.additionalProperties.properties) {
    return { props: { "<key>": s.additionalProperties }, required: [] };
  }
  return null;
}

function typeLabel(s) {
  if (!s) return "";
  if (s["x-kubernetes-int-or-string"]) return "int | string";
  if (s.type === "array") return "[]" + typeLabel(s.items);
  if (s.type === "object" || (!s.type && s.additionalProperties)) {
    if (s.properties) return "object";
    if (s.additionalProperties === true || (s.additionalProperties && !s.additionalProperties.type && !s.additionalProperties.properties)) return "map[string]any";
    if (s.additionalProperties) return "map[string]" + typeLabel(s.additionalProperties);
    if (s["x-kubernetes-preserve-unknown-fields"]) return "any";
    return "object";
  }
  if (s.format && s.format !== "int32" && s.format !== "int64") return `${s.type} (${s.format})`;
  if (s.format) return s.format;
  return s.type || "any";
}

const fmtValue = (v) => (typeof v === "string" ? v : JSON.stringify(v));

// Walk a kind's schema, calling fn(path[], schema, required, parentPath[]) for every field below spec/status.
function walk(kind, fn) {
  const rec = (s, path, req) => {
    const c = children(s);
    if (!c) return;
    for (const [name, sub] of Object.entries(c.props)) {
      if (path.length === 0 && SKIP_TOP.has(name)) continue;
      const p = [...path, name];
      fn(p, sub, c.required.includes(name));
      rec(sub, p);
    }
  };
  rec(kind.schema, []);
}

function findField(kind, path) {
  let s = kind.schema;
  let required = false;
  for (const seg of path) {
    const c = children(s);
    if (!c || !c.props[seg]) return null;
    required = c.required.includes(seg);
    s = c.props[seg];
  }
  return { schema: s, required };
}

function facts(s, required) {
  const rows = [["type", esc(typeLabel(s))]];
  if (required) rows.push(["required", "yes"]);
  if (s.default !== undefined) rows.push(["default", esc(fmtValue(s.default))]);
  if (s.enum) rows.push(["one of", s.enum.map((e) => `<span class="enum">${esc(fmtValue(e))}</span>`).join("")]);
  const items = s.type === "array" ? s.items || {} : {};
  if (items.enum) rows.push(["items", items.enum.map((e) => `<span class="enum">${esc(fmtValue(e))}</span>`).join("")]);
  for (const [k, label] of [["minimum", "minimum"], ["maximum", "maximum"], ["minLength", "min length"], ["maxLength", "max length"], ["minItems", "min items"], ["maxItems", "max items"], ["pattern", "pattern"]]) {
    if (s[k] !== undefined) rows.push([label, esc(s[k])]);
  }
  for (const v of s["x-kubernetes-validations"] || []) rows.push(["rule", esc(v.message || v.rule)]);
  if (s["x-kubernetes-list-type"]) rows.push(["list type", esc(s["x-kubernetes-list-type"])]);
  return `<dl class="facts">${rows.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("")}</dl>`;
}

function setVersion(data) {
  const el = document.querySelector("[data-version]");
  if (el && data.chartVersion) el.textContent = `v${data.chartVersion}`;
}

function copyButton(btn, getText) {
  btn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(getText());
      const old = btn.textContent;
      btn.textContent = "Copied";
      setTimeout(() => (btn.textContent = old), 1400);
    } catch {
      btn.textContent = "Copy failed";
    }
  });
}

// ---------- landing: live manifest ----------

async function initLanding() {
  document.querySelectorAll("[data-copy]").forEach((btn) => {
    const target = document.getElementById(btn.dataset.copy);
    copyButton(btn, () => target.innerText.replace(/^\$ /gm, ""));
  });

  const pre = document.getElementById("manifest");
  if (!pre) return;
  const peek = document.getElementById("peek");
  const data = await loadData().catch(() => null);
  if (data) setVersion(data);
  const kind = data?.kinds.find((k) => k.kind === pre.dataset.kind);
  pre.innerHTML = highlightManifest(pre.textContent, kind);

  const show = (a) => {
    const f = kind && findField(kind, a.dataset.path.split("."));
    if (!f) return;
    pre.querySelectorAll(".k.active").forEach((x) => x.classList.remove("active"));
    a.classList.add("active");
    peek.innerHTML = `<div class="peek-path">${esc(a.dataset.path)}<span class="peek-type">${esc(typeLabel(f.schema))}${f.schema.default !== undefined ? " = " + esc(fmtValue(f.schema.default)) : ""}</span></div><p>${inline(paragraphs(f.schema.description)[0] || "No description.")}</p>`;
    // Place the popover just below the hovered line, inside the manifest.
    const fig = pre.closest(".manifest");
    const kr = a.getBoundingClientRect(), fr = fig.getBoundingClientRect();
    peek.style.top = `${kr.bottom - fr.top + 6}px`;
    peek.classList.add("show");
  };
  const hide = () => {
    peek.classList.remove("show");
    pre.querySelectorAll(".k.active").forEach((x) => x.classList.remove("active"));
  };
  pre.addEventListener("mouseover", (e) => { const a = e.target.closest("a.k"); if (a) show(a); });
  pre.addEventListener("mouseleave", hide);
  pre.addEventListener("focusin", (e) => { const a = e.target.closest("a.k"); if (a) show(a); });
  pre.addEventListener("focusout", hide);
}

// Turns a YAML snippet into HTML where every key that exists in the CRD links to its API explorer entry.
function highlightManifest(text, kind) {
  const stack = []; // {indent, key}
  return text.replace(/\s+$/, "").split("\n").map((line) => {
    const m = line.match(/^(\s*)(- )?([A-Za-z0-9_.-]+)(:)(.*)$/);
    if (!m) {
      const c = line.match(/^(\s*)(- )?(.*)$/);
      return esc(c[1]) + (c[2] ? `<span class="p">- </span>` : "") + valueHTML(c[3]);
    }
    const [, ws, dash, key, colon, rest] = m;
    const indent = ws.length + (dash ? 2 : 0);
    while (stack.length && stack[stack.length - 1].indent >= indent) stack.pop();
    stack.push({ indent, key });
    const path = stack.map((s) => s.key).join(".");
    const known = kind && !SKIP_TOP.has(stack[0].key) && findField(kind, path.split("."));
    const keyHTML = known
      ? `<a class="k" href="api.html#${esc(kind.kind)}.${esc(path)}" data-path="${esc(path)}">${esc(key)}</a>`
      : `<span class="k-plain">${esc(key)}</span>`;
    return esc(ws) + (dash ? `<span class="p">- </span>` : "") + keyHTML + `<span class="p">${colon}</span>` + valueHTML(rest);
  }).join("\n");
}

function valueHTML(rest) {
  const i = rest.indexOf(" #");
  const [val, comment] = i >= 0 ? [rest.slice(0, i), rest.slice(i)] : [rest.startsWith("#") ? "" : rest, rest.startsWith("#") ? rest : ""];
  return (val.trim() ? `<span class="v">${esc(val)}</span>` : esc(val)) + (comment ? `<span class="c">${esc(comment)}</span>` : "");
}

// ---------- API explorer ----------

async function initAPI() {
  const main = document.getElementById("main");
  const side = document.getElementById("side-kinds");
  const results = document.getElementById("results");
  const search = document.getElementById("search");
  let data;
  try {
    data = await loadData();
  } catch (e) {
    main.innerHTML = `<p class="loading">Couldn't load the API data (${esc(e.message)}). Reload the page, or read the API reference in the <a href="docs.html#api-reference-api-reference">docs</a>.</p>`;
    return;
  }
  setVersion(data);

  // Search index
  const index = [];
  for (const k of data.kinds) {
    walk(k, (path, s) => index.push({ id: `${k.kind}.${path.join(".")}`, kind: k.kind, path: path.join("."), desc: s.description || "", hay: `${k.kind}.${path.join(".")} ${s.description || ""}`.toLowerCase() }));
  }

  // Sidebar
  side.innerHTML = data.kinds.map((k) => {
    const c = children(k.schema);
    const tops = ["spec", "status"].filter((t) => c.props[t]);
    const outline = tops.flatMap((t) => {
      const cc = children(c.props[t]);
      return [`<li><a href="#${k.kind}.${t}">${t}</a></li>`, ...(t === "spec" && cc ? Object.keys(cc.props).map((f) => `<li><a href="#${k.kind}.${t}.${f}">&nbsp;&nbsp;${esc(f)}</a></li>`) : [])];
    }).join("");
    return `<li><a class="side-kind" href="#${k.kind}" data-kind="${k.kind}">${k.kind}</a><ul class="side-outline">${outline}</ul></li>`;
  }).join("");

  let current = null;
  const render = (kind) => {
    current = kind;
    side.querySelectorAll(".side-kind").forEach((a) => a.setAttribute("aria-current", String(a.dataset.kind === kind.kind)));
    const cols = kind.printerColumns.map((c) => c.name).join(", ");
    main.innerHTML = `
      <header class="kind-head">
        <h1>${kind.kind}</h1>
        <div class="kind-meta">
          <span>apiVersion <code>${kind.group}/${kind.version}</code></span>
          <span>short name <code>${kind.shortNames.join(", ") || "–"}</code></span>
          <span>scope <code>${kind.scope}</code></span>
          <span><code>kubectl get ${kind.shortNames[0] || kind.plural}</code></span>
        </div>
        ${paragraphs(kind.schema.description).map((p) => `<p class="kind-desc">${inline(p)}</p>`).join("")}
        ${cols ? `<div class="columns">Printer columns: ${esc(cols)}</div>` : ""}
      </header>
      <div class="tools">
        <button class="tool" data-act="expand">Expand all</button>
        <button class="tool" data-act="collapse">Collapse all</button>
      </div>
      <div class="tree" role="tree">${treeHTML(kind, kind.schema, [], 0)}</div>`;
    document.title = `${kind.kind} · VoxKube API reference`;
  };

  main.addEventListener("click", (e) => {
    const tw = e.target.closest(".twisty");
    const key = e.target.closest(".key");
    const act = e.target.closest("[data-act]");
    const perm = e.target.closest("[data-permalink]");
    if (tw) toggleChildren(tw.closest("li"));
    else if (key) toggleDetail(key.closest("li"));
    else if (act) setAll(main, act.dataset.act === "expand");
    else if (perm) {
      e.preventDefault();
      const url = `${location.origin}${location.pathname}#${perm.dataset.permalink}`;
      history.replaceState(null, "", `#${perm.dataset.permalink}`);
      navigator.clipboard?.writeText(url).then(() => { perm.textContent = "Link copied"; setTimeout(() => (perm.textContent = "Copy link"), 1400); }, () => {});
    }
  });

  const route = () => {
    const id = decodeURIComponent(location.hash.slice(1));
    const kindName = id.split(".")[0];
    const kind = data.kinds.find((k) => k.kind === kindName) || data.kinds[0];
    if (current !== kind) render(kind);
    if (id.includes(".")) reveal(main, id);
    else if (id) window.scrollTo({ top: 0 });
  };
  window.addEventListener("hashchange", route);
  route();

  // Search
  const doSearch = () => {
    const q = search.value.trim().toLowerCase();
    side.hidden = !!q;
    results.hidden = !q;
    if (!q) return;
    const terms = q.split(/\s+/);
    const hits = index.filter((x) => terms.every((t) => x.hay.includes(t)))
      .sort((a, b) => (b.path.toLowerCase().includes(terms[0]) - a.path.toLowerCase().includes(terms[0])) || a.id.length - b.id.length)
      .slice(0, 60);
    results.innerHTML = hits.length
      ? hits.map((h) => `<li><a href="#${esc(h.id)}"><div class="r-path">${esc(h.kind)}.${esc(h.path)}</div><div class="r-desc">${esc(firstSentence(h.desc))}</div></a></li>`).join("")
      : `<li class="results-empty">No field matches “${esc(search.value)}”. Try a shorter word, like <code>git</code> or <code>route</code>.</li>`;
  };
  search.addEventListener("input", doSearch);
  search.addEventListener("keydown", (e) => {
    if (e.key === "Enter") results.querySelector("a")?.click();
    if (e.key === "Escape") { search.value = ""; doSearch(); }
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "/" && document.activeElement !== search) { e.preventDefault(); search.focus(); }
  });
}

function treeHTML(kind, schema, path, depth) {
  const c = children(schema);
  if (!c) return "";
  const entries = Object.entries(c.props).filter(([n]) => !(depth === 0 && SKIP_TOP.has(n)));
  const items = entries.map(([name, s]) => {
    const p = [...path, name];
    const id = `${kind.kind}.${p.join(".")}`;
    const hasKids = !!children(s);
    const open = depth === 0;
    const req = c.required.includes(name);
    return `<li role="treeitem" data-id="${esc(id)}">
      <div class="row">
        ${hasKids ? `<button class="twisty" aria-expanded="${open}" aria-label="Show fields of ${esc(name)}">▸</button>` : "<span></span>"}
        <button class="key" aria-expanded="false">${esc(name)}</button>
        <span class="sum"><span class="t">${esc(typeLabel(s))}</span>${req ? '<span class="req">required</span>' : ""}${s.default !== undefined ? `<span class="def">${esc(fmtValue(s.default))}</span>` : ""}<span class="brief">${esc(firstSentence(s.description))}</span></span>
      </div>
      <div class="detail" hidden>
        ${paragraphs(s.description).map((x) => `<p>${inline(x)}</p>`).join("") || "<p>No description.</p>"}
        ${facts(s, req)}
        <a class="permalink" href="#${esc(id)}" data-permalink="${esc(id)}">Copy link</a>
      </div>
      ${hasKids ? `<ul role="group"${open ? "" : " hidden"}>${treeHTML(kind, s, p, depth + 1)}</ul>` : ""}
    </li>`;
  });
  return `<ul>${items.join("")}</ul>`;
}

function toggleChildren(li, force) {
  const tw = li.querySelector(":scope > .row > .twisty");
  const ul = li.querySelector(":scope > ul");
  if (!tw || !ul) return;
  const open = force ?? tw.getAttribute("aria-expanded") !== "true";
  tw.setAttribute("aria-expanded", String(open));
  ul.hidden = !open;
}

function toggleDetail(li, force) {
  const key = li.querySelector(":scope > .row > .key");
  const det = li.querySelector(":scope > .detail");
  const open = force ?? key.getAttribute("aria-expanded") !== "true";
  key.setAttribute("aria-expanded", String(open));
  det.hidden = !open;
  if (open) toggleChildren(li, true);
}

function setAll(root, open) {
  root.querySelectorAll(".tree li").forEach((li) => toggleChildren(li, open));
}

function reveal(root, id) {
  const li = root.querySelector(`li[data-id="${CSS.escape(id)}"]`);
  if (!li) return;
  for (let p = li.parentElement.closest("li"); p; p = p.parentElement.closest("li")) toggleChildren(p, true);
  toggleDetail(li, true);
  const row = li.querySelector(":scope > .row");
  row.scrollIntoView({ block: "center" });
  row.classList.remove("flash");
  void row.offsetWidth;
  row.classList.add("flash");
}

// ---------- docs: README ----------

async function initDocs() {
  const out = document.getElementById("doc");
  let md;
  try {
    const r = await fetch("README.md");
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    md = await r.text();
  } catch (e) {
    out.innerHTML = `<p class="loading">Couldn't load the README (${esc(e.message)}). Read it <a href="${REPO}#readme">on GitHub</a>.</p>`;
    return;
  }
  loadData().then(setVersion, () => {});
  const { marked } = await import("https://cdn.jsdelivr.net/npm/marked@15.0.12/lib/marked.esm.js");

  const seen = new Map();
  const slug = (raw) => {
    const base = raw.replace(/<[^>]+>/g, "").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").replace(/`/g, "")
      .toLowerCase().trim().replace(/[^\p{L}\p{N}\s_-]/gu, "").replace(/\s/g, "-");
    const n = seen.get(base) || 0;
    seen.set(base, n + 1);
    return n ? `${base}-${n}` : base;
  };
  marked.use({
    renderer: {
      heading({ tokens, depth, text }) {
        return `<h${depth} id="${slug(text)}">${this.parser.parseInline(tokens)}</h${depth}>\n`;
      },
      code({ text, lang }) {
        if (lang === "mermaid") return `<pre class="mermaid">${esc(text)}</pre>`;
        return false;
      },
    },
  });
  out.innerHTML = marked.parse(md);

  // GitHub alerts (> [!NOTE]) become callouts.
  out.querySelectorAll("blockquote").forEach((bq) => {
    const p = bq.querySelector("p");
    const m = p && p.innerHTML.match(/^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*(<br>)?/);
    if (m) {
      bq.classList.add(`callout-${m[1].toLowerCase()}`);
      p.innerHTML = p.innerHTML.slice(m[0].length);
    }
  });
  // Relative links to repository files point to GitHub.
  out.querySelectorAll("a[href]").forEach((a) => {
    const h = a.getAttribute("href");
    if (!/^(https?:|#|mailto:)/.test(h)) a.href = `${REPO}/blob/main/${h}`;
  });
  // Point the README's API reference to the explorer.
  const api = out.querySelector("#api-reference-api-reference");
  if (api) api.insertAdjacentHTML("afterend", `<p class="api-pointer">Browse these fields interactively, with search and links to every field, in the <a href="api.html">API explorer</a>.</p>`);

  if (out.querySelector(".mermaid")) {
    const { default: mermaid } = await import("https://cdn.jsdelivr.net/npm/mermaid@11.4.1/dist/mermaid.esm.min.mjs");
    const dark = matchMedia("(prefers-color-scheme: dark)").matches;
    mermaid.initialize({ startOnLoad: false, theme: dark ? "dark" : "neutral", fontFamily: "IBM Plex Sans, sans-serif" });
    await mermaid.run({ nodes: out.querySelectorAll(".mermaid") });
  }
  if (location.hash) document.getElementById(decodeURIComponent(location.hash.slice(1)))?.scrollIntoView();
}

// ---------- boot ----------

const page = document.body.dataset.page;
if (page === "landing") initLanding();
else if (page === "api") initAPI();
else if (page === "docs") initDocs();
