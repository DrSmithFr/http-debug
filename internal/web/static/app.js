/* HTTP Debug Proxy — UI. No framework, no build step. */
(() => {
  "use strict";

  const state = {
    rows: new Map(),   // id -> list row
    order: [],         // ids, newest first
    selected: null,    // currently opened id
    detail: null,      // detail payload of the selected entry
    tab: "response",
    live: { id: null, chunks: [] }, // fragments received while streaming
    bodyCache: new Map(),           // "<id>:<side>" -> body fetched on demand
    filter: "",
  };

  const el = {
    list: document.getElementById("list"),
    empty: document.getElementById("list-empty"),
    count: document.getElementById("count"),
    filter: document.getElementById("filter"),
    layout: document.getElementById("layout"),
    pane: document.getElementById("detail-pane"),
    detail: document.getElementById("detail"),
    target: document.getElementById("target"),
    connection: document.getElementById("connection"),
    resizer: document.getElementById("resizer"),
  };

  // --- Helpers ---

  const api = async (path, options) => {
    const res = await fetch(path, options);
    if (!res.ok) {
      let message = res.statusText;
      try {
        message = (await res.json()).error || message;
      } catch (_) { /* body was not JSON */ }
      throw new Error(message);
    }
    return res.status === 204 ? null : res.json();
  };

  const pathOf = (raw) => {
    try {
      const u = new URL(raw);
      return u.pathname + u.search;
    } catch (_) {
      return raw;
    }
  };

  const fmtMs = (ms) => {
    if (ms === null || ms === undefined) return "";
    if (ms < 1000) return ms + " ms";
    return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + " s";
  };

  // A running timer needs a stable width, so the precision stays fixed rather
  // than following the magnitude the way fmtMs does.
  const fmtElapsed = (ms) => (ms < 1000 ? ms + " ms" : (ms / 1000).toFixed(1) + " s");

  // Elapsed counters are marked with data-elapsed-from and refreshed in place,
  // so a pending request shows how long it has been waiting without the list
  // being re-rendered ten times a second.
  const elapsedCounter = (startedAt, extraClass) =>
    elem("span", {
      class: "duration duration-pending" + (extraClass ? " " + extraClass : ""),
      "data-elapsed-from": startedAt,
      text: fmtElapsed(Math.max(0, Date.now() - startedAt)),
    });

  const tickElapsed = () => {
    const now = Date.now();
    for (const node of document.querySelectorAll("[data-elapsed-from]")) {
      node.textContent = fmtElapsed(Math.max(0, now - Number(node.dataset.elapsedFrom)));
    }
  };

  const fmtBytes = (n) => {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / (1024 * 1024)).toFixed(1) + " MB";
  };

  const statusClass = (row) => {
    if (row.status === "error") return "err";
    if (!row.status_code) return "";
    return String(Math.floor(row.status_code / 100));
  };

  const elem = (tag, props = {}, children = []) => {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(props)) {
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else if (v !== null && v !== undefined) node.setAttribute(k, v);
    }
    for (const child of [].concat(children)) {
      if (child) node.appendChild(child);
    }
    return node;
  };

  let toastTimer = null;
  const toast = (message) => {
    document.querySelectorAll(".toast").forEach((t) => t.remove());
    const node = elem("div", { class: "toast", text: message });
    document.body.appendChild(node);
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => node.remove(), 2200);
  };

  const copyText = async (text, button) => {
    try {
      await navigator.clipboard.writeText(text);
    } catch (_) {
      // Clipboard API needs a secure context; fall back to a hidden textarea.
      const ta = elem("textarea", { style: "position:fixed;opacity:0" });
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      ta.remove();
    }
    if (button) {
      const original = button.textContent;
      button.textContent = "copied";
      button.classList.add("done");
      setTimeout(() => {
        button.textContent = original;
        button.classList.remove("done");
      }, 1000);
    }
  };

  const copyButton = (getText, label = "copy") =>
    elem("button", {
      class: "copy",
      type: "button",
      title: "Copy value",
      text: label,
      onclick: (e) => {
        e.stopPropagation();
        copyText(getText(), e.currentTarget);
      },
    });

  // --- Request list ---

  const matchesFilter = (row) => {
    if (!state.filter) return true;
    const haystack = [row.method, row.url, row.status_code, row.status].join(" ").toLowerCase();
    return haystack.includes(state.filter);
  };

  const rowNode = (row) => {
    const badges = [];
    if (row.is_ollama) badges.push(elem("span", { class: "badge badge-ollama", text: "ollama" }));
    if (row.is_replay) badges.push(elem("span", { class: "badge badge-replay", text: "replay" }));

    const trailing = row.status === "pending"
      ? elem("span", { class: "pending-meta", title: "Waiting for the response" }, [
          elem("span", { class: "spinner" }),
          elapsedCounter(row.started_at),
        ])
      : elem("span", { class: "duration", text: fmtMs(row.total_ms) });

    return elem("li", {
      class: "row" + (row.id === state.selected ? " selected" : ""),
      "data-id": row.id,
      onclick: () => select(row.id),
    }, [
      elem("span", { class: "method", "data-method": row.method, text: row.method }),
      elem("span", {
        class: "status",
        "data-class": statusClass(row),
        text: row.status === "error" ? "err" : (row.status_code || ""),
        title: row.error || "",
      }),
      elem("span", { class: "path", text: pathOf(row.url), title: row.url }),
      elem("span", { class: "meta" }, [...badges, trailing]),
    ]);
  };

  const renderList = () => {
    const visible = state.order.map((id) => state.rows.get(id)).filter(Boolean).filter(matchesFilter);
    el.list.replaceChildren(...visible.map(rowNode));
    el.empty.hidden = visible.length > 0;
    el.count.textContent = visible.length
      ? visible.length + (visible.length === state.order.length ? "" : " / " + state.order.length)
      : "";
  };

  const upsertRow = (row) => {
    const known = state.rows.has(row.id);
    state.rows.set(row.id, Object.assign(state.rows.get(row.id) || {}, row));
    if (!known) {
      state.order.unshift(row.id);
    }
  };

  const removeRow = (id) => {
    state.rows.delete(id);
    state.order = state.order.filter((x) => x !== id);
    if (state.selected === id) closeDetail();
  };

  // --- Detail ---

  const select = async (id, replaceHistory = false) => {
    state.selected = id;
    state.tab = state.tab === "edit" ? "response" : state.tab;
    state.live = { id, chunks: [] };
    renderList();
    el.layout.classList.add("split");
    el.pane.hidden = false;
    const url = "/r/" + id;
    if (location.pathname !== url) {
      history[replaceHistory ? "replaceState" : "pushState"]({ id }, "", url);
    }
    try {
      state.detail = await api("/api/requests/" + id);
      renderDetail();
    } catch (err) {
      el.detail.replaceChildren(elem("p", { class: "empty", text: "Could not load request: " + err.message }));
    }
  };

  const closeDetail = () => {
    state.selected = null;
    state.detail = null;
    el.pane.hidden = true;
    el.layout.classList.remove("split");
    if (location.pathname !== "/") history.pushState({}, "", "/");
    renderList();
  };

  // A metric with no value yet is omitted entirely rather than rendered as a
  // dangling label: a pending entry has no TTFB, stream or total duration.
  const metric = (label, value) =>
    value === null || value === undefined || value === ""
      ? null
      : elem("span", { class: "metric" }, [
          document.createTextNode(label + " "),
          elem("b", { text: value }),
        ]);

  const renderDetail = () => {
    const d = state.detail;
    if (!d) return;

    const node = document.getElementById("tpl-detail").content.cloneNode(true);
    const slot = (name) => node.querySelector(`[data-slot="${name}"]`);

    const method = slot("method");
    method.textContent = d.method;
    method.setAttribute("data-method", d.method);

    const status = slot("status");
    status.textContent = d.status === "error" ? "error" : (d.status_code || d.status);
    status.setAttribute("data-class", statusClass(d));

    slot("url").textContent = d.url;
    node.querySelector('[data-copy-slot="url"]').addEventListener("click", (e) => copyText(d.url, e.currentTarget));

    const metrics = [
      metric("TTFB", fmtMs(d.ttfb_ms)),
      metric("Stream", fmtMs(d.stream_ms)),
      metric("Total", fmtMs(d.total_ms)),
      metric("Started", new Date(d.started_at).toLocaleTimeString()),
    ].filter(Boolean);
    if (d.status === "pending") {
      metrics.unshift(elem("span", { class: "metric" }, [
        document.createTextNode("Elapsed "),
        elapsedCounter(d.started_at, "metric-elapsed"),
      ]));
    }
    slot("metrics").replaceChildren(...metrics);

    slot("permalink").setAttribute("href", "/r/" + d.id);
    node.querySelector('[data-action="close"]').addEventListener("click", closeDetail);
    node.querySelector('[data-action="curl"]').addEventListener("click", (e) => {
      copyText(toCurl(d), e.currentTarget);
      toast("curl command copied to clipboard");
    });
    node.querySelector('[data-action="edit"]').addEventListener("click", () => {
      state.tab = "edit";
      renderDetail();
    });

    if (d.error) {
      slot("metrics").appendChild(elem("span", { class: "metric composer-error", text: d.error }));
    }

    const tabs = [
      { id: "request", label: "Request" },
      { id: "response", label: "Response" },
    ];
    if (d.is_ollama) tabs.push({ id: "ollama", label: "Ollama" });
    tabs.push({ id: "edit", label: "Edit & send" });
    if (!tabs.some((t) => t.id === state.tab)) state.tab = "response";

    slot("tabs").replaceChildren(...tabs.map((t) =>
      elem("button", {
        class: "tab" + (t.id === state.tab ? " active" : ""),
        type: "button",
        text: t.label,
        onclick: () => { state.tab = t.id; renderDetail(); },
      })
    ));

    slot("body").replaceChildren(renderTab(d));
    el.detail.replaceChildren(node);
  };

  const renderTab = (d) => {
    switch (state.tab) {
      case "request": return renderSide(d, "request");
      case "ollama": return renderOllama(d);
      case "edit": return renderComposer(d);
      default: return renderSide(d, "response");
    }
  };

  const headerTable = (headers) => {
    const names = Object.keys(headers || {}).sort();
    if (!names.length) return elem("p", { class: "empty", text: "No headers." });
    return elem("table", { class: "kv" }, names.map((name) =>
      elem("tr", {}, [
        elem("td", { class: "k", text: name }),
        elem("td", { class: "v", text: headers[name] }),
        elem("td", { class: "c" }, [copyButton(() => headers[name])]),
      ])
    ));
  };

  const queryTable = (rawUrl) => {
    let params;
    try {
      params = [...new URL(rawUrl).searchParams.entries()];
    } catch (_) {
      return null;
    }
    if (!params.length) return null;
    return elem("div", {}, [
      elem("h3", { class: "section-title", text: "Query parameters" }),
      elem("table", { class: "kv" }, params.map(([k, v]) =>
        elem("tr", {}, [
          elem("td", { class: "k", text: k }),
          elem("td", { class: "v", text: v }),
          elem("td", { class: "c" }, [copyButton(() => v)]),
        ])
      )),
    ]);
  };

  // liveBody returns the fragments accumulated since the entry was opened,
  // so a streaming response keeps growing on screen.
  const liveBody = (d, side) => {
    const stored = d[side].body || "";
    if (side !== "response" || state.live.id !== d.id || !state.live.chunks.length) return stored;
    return stored + state.live.chunks.join("");
  };

  const renderSide = (d, side) => {
    const payload = d[side];
    const body = liveBody(d, side);
    const container = elem("div");

    container.appendChild(elem("h3", { class: "section-title", text: "Headers" }));
    container.appendChild(headerTable(side === "request" ? d.request_headers : d.response_headers));

    if (side === "request") {
      const query = queryTable(d.url);
      if (query) container.appendChild(query);
    }

    container.appendChild(elem("h3", { class: "section-title", text: "Body" }));

    if (payload.binary) {
      container.appendChild(elem("p", { class: "empty" }, [
        document.createTextNode("Binary payload, " + fmtBytes(payload.size) + ". "),
        elem("a", { class: "btn btn-small", href: payload.url, text: "Download raw" }),
      ]));
      return container;
    }

    if (!body && !payload.size) {
      container.appendChild(elem("p", { class: "empty", text: "Empty body." }));
      return container;
    }

    // A body that spilled to disk is not inlined in the detail payload, so it
    // is fetched on demand rather than pulled in with every request opened.
    if (!body && payload.size) {
      const box = elem("div");
      const load = elem("button", {
        class: "btn btn-small", type: "button", text: "Load " + fmtBytes(payload.size),
      });
      load.addEventListener("click", async () => {
        load.disabled = true;
        load.textContent = "Loading…";
        try {
          const res = await fetch(payload.url);
          if (!res.ok) throw new Error(res.statusText);
          box.replaceChildren(elem("pre", { class: "raw", text: await res.text() }));
          load.remove();
        } catch (err) {
          box.replaceChildren(elem("p", { class: "composer-error", text: "Could not load body: " + err.message }));
          load.disabled = false;
          load.textContent = "Retry";
        }
      });
      container.appendChild(elem("div", { class: "body-toolbar" }, [
        load,
        elem("a", { class: "btn btn-small", href: payload.url, text: "Raw file" }),
        elem("span", {
          class: "body-info",
          text: [payload.format || "raw", fmtBytes(payload.size), "stored on disk"].join(" · "),
        }),
      ]));
      container.appendChild(box);
      return container;
    }

    const structured = payload.format === "json" || payload.format === "xml";
    const view = { mode: structured ? "tree" : "raw" };
    const bodyBox = elem("div");

    const draw = () => {
      let rendered = null;
      if (view.mode === "tree") rendered = buildTree(payload.format, body);
      bodyBox.replaceChildren(rendered || elem("pre", { class: "raw", text: body }));
    };

    const toolbar = elem("div", { class: "body-toolbar" });
    if (structured) {
      const toggle = elem("button", { class: "btn btn-small", type: "button", text: "Raw" });
      toggle.addEventListener("click", () => {
        view.mode = view.mode === "tree" ? "raw" : "tree";
        toggle.textContent = view.mode === "tree" ? "Raw" : "Tree";
        draw();
      });
      toolbar.appendChild(toggle);
    }
    toolbar.appendChild(elem("button", {
      class: "btn btn-small", type: "button", text: "Copy body",
      onclick: (e) => copyText(body, e.currentTarget),
    }));
    toolbar.appendChild(elem("a", { class: "btn btn-small", href: payload.url, text: "Raw file" }));

    const info = [payload.format || "raw", fmtBytes(payload.size)];
    if (payload.truncated) info.push("preview only — open the raw file for the full payload");
    toolbar.appendChild(elem("span", { class: "body-info", text: info.join(" · ") }));

    container.appendChild(toolbar);
    draw();
    container.appendChild(bodyBox);
    return container;
  };

  // --- Structured body views ---

  const buildTree = (format, body) => {
    if (format === "json") {
      try {
        return elem("div", { class: "tree" }, [jsonNode(null, JSON.parse(body), "$")]);
      } catch (_) {
        return null; // not valid JSON after all; the raw view is the honest one
      }
    }
    if (format === "xml") {
      const doc = new DOMParser().parseFromString(body, "application/xml");
      if (doc.querySelector("parsererror") || !doc.documentElement) return null;
      return elem("div", { class: "tree" }, [xmlNode(doc.documentElement)]);
    }
    return null;
  };

  const scalarNode = (value) => {
    const type = value === null ? "null" : typeof value;
    const text = type === "string" ? JSON.stringify(value) : String(value);
    return elem("span", { class: "val-" + type, text });
  };

  const jsonNode = (key, value, path) => {
    const node = elem("div", { class: "node" });
    const label = key === null
      ? null
      : elem("span", {}, [
          elem("span", { class: "node-key", text: JSON.stringify(key) }),
          elem("span", { class: "node-punct", text: ": " }),
        ]);

    if (value === null || typeof value !== "object") {
      node.append(...[label, scalarNode(value)].filter(Boolean));
      node.appendChild(copyButton(() => (typeof value === "string" ? value : JSON.stringify(value))));
      return node;
    }

    const isArray = Array.isArray(value);
    const entries = isArray ? value.map((v, i) => [i, v]) : Object.entries(value);
    const details = elem("details", entries.length <= 100 ? { open: "" } : {});
    const summary = elem("summary", {}, [
      label,
      elem("span", { class: "node-punct", text: isArray ? "[" : "{" }),
      elem("span", { class: "node-count", text: " " + entries.length + (isArray ? " items" : " keys") + " " }),
      elem("span", { class: "node-punct", text: isArray ? "]" : "}" }),
    ].filter(Boolean));

    details.appendChild(summary);
    for (const [k, v] of entries) {
      details.appendChild(jsonNode(isArray ? k : k, v, path + "." + k));
    }
    node.appendChild(details);
    node.appendChild(copyButton(() => JSON.stringify(value, null, 2)));
    return node;
  };

  const xmlNode = (element) => {
    const node = elem("div", { class: "node" });
    const attrs = [...element.attributes].map((a) => " " + a.name + '="' + a.value + '"').join("");
    const children = [...element.children];

    if (!children.length) {
      const text = element.textContent.trim();
      node.append(
        elem("span", { class: "node-key", text: "<" + element.nodeName + attrs + ">" }),
        elem("span", { class: "val-string", text: text }),
        elem("span", { class: "node-punct", text: "</" + element.nodeName + ">" }),
      );
      node.appendChild(copyButton(() => text));
      return node;
    }

    const details = elem("details", { open: "" });
    details.appendChild(elem("summary", {}, [
      elem("span", { class: "node-key", text: "<" + element.nodeName + attrs + ">" }),
      elem("span", { class: "node-count", text: " " + children.length + " children" }),
    ]));
    children.forEach((child) => details.appendChild(xmlNode(child)));
    node.appendChild(details);
    node.appendChild(copyButton(() => new XMLSerializer().serializeToString(element)));
    return node;
  };

  // --- Ollama message preview ---

  // decodeOllama mirrors the server-side decoder: it concatenates `response`
  // (/api/generate) or `message.content` (/api/chat) across fragments.
  const decodeOllama = (format, body) => {
    let out = "";
    for (const raw of body.split("\n")) {
      let line = raw.trim();
      if (!line) continue;
      if (format === "sse") {
        if (!line.startsWith("data:")) continue;
        line = line.slice(5).trim();
        if (line === "[DONE]") continue;
      }
      try {
        const fragment = JSON.parse(line);
        out += fragment.response || fragment.message?.content || "";
      } catch (_) { /* partial or non-JSON fragment */ }
    }
    return out;
  };

  const parseJSON = (text) => {
    try {
      return JSON.parse(text);
    } catch (_) {
      return null;
    }
  };

  // A body that spilled to disk is not inlined in the detail payload. It is
  // fetched once on demand and kept, so switching tabs does not refetch it.
  const cachedBody = (d, side) => {
    if (d[side].body) return d[side].body;
    const cached = state.bodyCache.get(d.id + ":" + side);
    return cached === undefined ? null : cached;
  };

  const loadBodyButton = (d, side) => {
    const payload = d[side];
    const button = elem("button", {
      class: "btn btn-small", type: "button",
      text: "Load " + side + " body (" + fmtBytes(payload.size) + ")",
    });
    button.addEventListener("click", async () => {
      button.disabled = true;
      button.textContent = "Loading…";
      try {
        const res = await fetch(payload.url);
        if (!res.ok) throw new Error(res.statusText);
        state.bodyCache.set(d.id + ":" + side, await res.text());
        renderDetail();
      } catch (err) {
        button.disabled = false;
        button.textContent = "Retry";
        toast("Could not load body: " + err.message);
      }
    });
    return elem("div", { class: "body-toolbar" }, [
      button,
      elem("span", { class: "body-info", text: "stored on disk" }),
    ]);
  };

  const section = (title, badge, children, open = false) => {
    const details = elem("details", open ? { class: "section", open: "" } : { class: "section" });
    details.appendChild(elem("summary", {}, [
      elem("span", { class: "section-heading", text: title }),
      badge ? elem("span", { class: "node-count", text: " " + badge }) : null,
    ].filter(Boolean)));
    details.appendChild(elem("div", { class: "section-body" }, children));
    return details;
  };

  const plural = (n, word) => n + " " + word + (n > 1 ? "s" : "");

  // Message content is a plain string on Ollama, but the OpenAI-compatible
  // shape allows an array of parts.
  const contentText = (content) => {
    if (content === null || content === undefined) return "";
    if (typeof content === "string") return content;
    if (Array.isArray(content)) {
      return content.map((part) => (typeof part === "string" ? part : part.text || "")).join("");
    }
    return JSON.stringify(content, null, 2);
  };

  // /api/chat carries a messages array; /api/generate carries a flat prompt,
  // which is shown here in the same conversation shape.
  const conversationOf = (payload) => {
    if (Array.isArray(payload.messages)) return payload.messages;
    const messages = [];
    if (payload.system) messages.push({ role: "system", content: payload.system });
    if (payload.prompt) messages.push({ role: "user", content: payload.prompt, images: payload.images });
    return messages;
  };

  // MCP tools reach a model in several shapes depending on the client:
  // declared at the top level, sent as a tool of type "mcp", or flattened into
  // the tools array under an `mcp__<server>__<tool>` name. All three are folded
  // into one server list here.
  const splitTools = (payload) => {
    const servers = new Map();
    const functions = [];

    const serverFor = (label, url) => {
      const key = label || "mcp";
      if (!servers.has(key)) servers.set(key, { label: key, url: url || "", tools: [] });
      const entry = servers.get(key);
      if (url && !entry.url) entry.url = url;
      return entry;
    };

    const declared = payload.mcp_servers || payload.mcpServers;
    if (declared) {
      const list = Array.isArray(declared)
        ? declared
        : Object.entries(declared).map(([name, cfg]) => Object.assign({ name }, cfg));
      for (const entry of list) {
        serverFor(
          entry.name || entry.server_label || entry.label,
          entry.url || entry.server_url || entry.command,
        );
      }
    }

    for (const tool of payload.tools || []) {
      if (tool.type === "mcp") {
        const server = serverFor(tool.server_label || tool.name, tool.server_url);
        for (const name of tool.allowed_tools || []) server.tools.push({ name, description: "" });
        continue;
      }
      const fn = tool.function || tool;
      const name = fn.name || "(unnamed)";
      const spec = {
        name,
        description: fn.description || "",
        parameters: fn.parameters || fn.input_schema || null,
      };
      if (name.startsWith("mcp__")) {
        const parts = name.slice("mcp__".length).split("__");
        if (parts.length >= 2) {
          serverFor(parts[0]).tools.push(Object.assign({}, spec, { name: parts.slice(1).join("__") }));
          continue;
        }
      }
      functions.push(spec);
    }
    return { functions, servers: [...servers.values()] };
  };

  const toolNode = (tool) => {
    const parts = [
      elem("div", { class: "card-head" }, [
        elem("span", { class: "node-key", text: tool.name }),
        copyButton(() => tool.name),
      ]),
    ];
    if (tool.description) parts.push(elem("p", { class: "card-desc", text: tool.description }));
    if (tool.parameters) {
      parts.push(elem("details", { class: "card-params" }, [
        elem("summary", { text: "parameters" }),
        elem("div", { class: "tree" }, [jsonNode(null, tool.parameters, "$")]),
      ]));
    }
    return elem("div", { class: "card" }, parts);
  };

  const serverNode = (server) => {
    const parts = [
      elem("div", { class: "card-head" }, [
        elem("span", { class: "badge badge-mcp", text: "mcp" }),
        elem("span", { class: "node-key", text: server.label }),
        server.url ? copyButton(() => server.url) : null,
      ].filter(Boolean)),
    ];
    if (server.url) parts.push(elem("p", { class: "card-desc card-mono", text: server.url }));
    parts.push(server.tools.length
      ? elem("div", { class: "card-nested" }, server.tools.map(toolNode))
      : elem("p", { class: "body-info", text: "No tool listed for this server." }));
    return elem("div", { class: "card" }, parts);
  };

  const toolCallNode = (call) => {
    const fn = call.function || call;
    let args = fn.arguments !== undefined ? fn.arguments : fn.input;
    if (typeof args === "string") {
      const parsed = parseJSON(args);
      if (parsed !== null) args = parsed;
    }
    const box = elem("div", { class: "tool-call" }, [
      elem("div", { class: "card-head" }, [
        elem("span", { class: "call-arrow", text: "→" }),
        elem("span", { class: "node-key", text: fn.name || "(unnamed)" }),
        copyButton(() => JSON.stringify(fn, null, 2)),
      ]),
    ]);
    if (args !== undefined && args !== null && args !== "") {
      box.appendChild(typeof args === "object"
        ? elem("div", { class: "tree" }, [jsonNode(null, args, "$")])
        : elem("pre", { class: "raw", text: String(args) }));
    }
    return box;
  };

  const messageNode = (msg, index) => {
    const role = String(msg.role || "?").toLowerCase();
    const text = contentText(msg.content);
    const parts = [
      elem("div", { class: "card-head" }, [
        elem("span", { class: "role", "data-role": role, text: role }),
        msg.tool_name ? elem("span", { class: "node-count", text: msg.tool_name }) : null,
        elem("span", { class: "node-count", text: "#" + (index + 1) }),
        text ? copyButton(() => text) : null,
      ].filter(Boolean)),
    ];
    if (text) parts.push(elem("pre", { class: "raw preview msg-body", text }));
    if (Array.isArray(msg.images) && msg.images.length) {
      parts.push(elem("p", { class: "body-info", text: plural(msg.images.length, "inline image") + ", not shown" }));
    }
    for (const call of msg.tool_calls || []) parts.push(toolCallNode(call));
    if (!text && !(msg.tool_calls || []).length) {
      parts.push(elem("p", { class: "body-info", text: "Empty message." }));
    }
    return elem("div", { class: "msg", "data-role": role }, parts);
  };

  const renderOllama = (d) => {
    const container = elem("div");

    // The assistant message, reconstructed live while the response streams.
    const body = liveBody(d, "response");
    const message = state.live.id === d.id && state.live.chunks.length
      ? decodeOllama(d.response.format, body)
      : (d.ollama_preview || decodeOllama(d.response.format, body));

    container.appendChild(section(
      "Response message",
      message ? plural(message.length, "character") : null,
      [message
        ? elem("div", {}, [
            elem("div", { class: "body-toolbar" }, [
              elem("button", {
                class: "btn btn-small", type: "button", text: "Copy message",
                onclick: (e) => copyText(message, e.currentTarget),
              }),
            ]),
            elem("pre", { class: "raw preview", text: message }),
          ])
        : elem("p", { class: "empty", text: "No message content decoded from this response yet." })],
      true,
    ));

    const raw = cachedBody(d, "request");
    if (raw === null) {
      container.appendChild(section("Request payload", null, [
        elem("p", { class: "body-info", text: "Load the request body to inspect the conversation, the tools and the MCP servers." }),
        loadBodyButton(d, "request"),
      ], true));
      return container;
    }

    const payload = parseJSON(raw);
    if (!payload || typeof payload !== "object") {
      container.appendChild(elem("p", { class: "empty", text: "The request body is not a JSON object, so there is no conversation to show." }));
      return container;
    }

    const meta = [];
    if (payload.model) meta.push(["model", String(payload.model)]);
    if (payload.stream !== undefined) meta.push(["stream", String(payload.stream)]);
    if (payload.format) meta.push(["format", typeof payload.format === "string" ? payload.format : "schema"]);
    if (payload.keep_alive !== undefined) meta.push(["keep_alive", String(payload.keep_alive)]);
    if (meta.length) {
      container.appendChild(elem("table", { class: "kv" }, meta.map(([k, v]) =>
        elem("tr", {}, [
          elem("td", { class: "k", text: k }),
          elem("td", { class: "v", text: v }),
          elem("td", { class: "c" }, [copyButton(() => v)]),
        ])
      )));
    }

    const messages = conversationOf(payload);
    const { functions, servers } = splitTools(payload);

    if (messages.length) {
      container.appendChild(section("Conversation", plural(messages.length, "message"),
        messages.map(messageNode), true));
    }
    if (servers.length) {
      container.appendChild(section("MCP servers", plural(servers.length, "server"),
        servers.map(serverNode), true));
    }
    if (functions.length) {
      container.appendChild(section("Tools", plural(functions.length, "tool"),
        functions.map(toolNode), true));
    }
    if (payload.options && typeof payload.options === "object") {
      container.appendChild(section("Options", plural(Object.keys(payload.options).length, "key"),
        [elem("div", { class: "tree" }, [jsonNode(null, payload.options, "$")])]));
    }
    if (!messages.length && !functions.length && !servers.length) {
      container.appendChild(elem("p", { class: "empty", text: "No conversation, tool or MCP server found in this request payload." }));
    }
    return container;
  };

  // --- Composer (replay by editing) ---

  // The composer exists to be edited by hand, so a JSON body is pre-filled
  // indented rather than as the single line the client actually sent. Anything
  // that does not parse as a JSON object or array is left exactly as captured:
  // NDJSON, SSE and plain text all lose their meaning if reflowed.
  const editableBody = (d) => {
    if (!d) return "";
    const raw = cachedBody(d, "request");
    if (raw === null || !raw.trim()) return raw || "";
    const parsed = parseJSON(raw);
    if (parsed === null || typeof parsed !== "object") return raw;
    return JSON.stringify(parsed, null, 2);
  };

  const renderComposer = (d) => {
    const headers = Object.entries(d ? d.request_headers || {} : {})
      .map(([k, v]) => k + ": " + v)
      .join("\n");

    const method = elem("select", {});
    for (const m of ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]) {
      method.appendChild(elem("option", { value: m, text: m }));
    }
    method.value = d ? d.method : "GET";

    const url = elem("input", { type: "url", value: d ? d.url : "", placeholder: "https://host/path" });
    const headersField = elem("textarea", { style: "min-height:110px", placeholder: "Header-Name: value" });
    headersField.value = headers;
    const bodyField = elem("textarea", { placeholder: "Request body" });
    bodyField.value = editableBody(d);

    const error = elem("p", { class: "composer-error" });
    const send = elem("button", { class: "btn btn-primary", type: "submit", text: "Send request" });

    const form = elem("form", {
      class: "composer",
      onsubmit: async (e) => {
        e.preventDefault();
        error.textContent = "";
        send.disabled = true;
        try {
          const parsed = {};
          for (const line of headersField.value.split("\n")) {
            const idx = line.indexOf(":");
            if (idx <= 0) continue;
            const name = line.slice(0, idx).trim();
            const value = line.slice(idx + 1).trim();
            if (!name) continue;
            (parsed[name] = parsed[name] || []).push(value);
          }
          const created = await api("/api/requests", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              method: method.value,
              url: url.value,
              headers: parsed,
              body: bodyField.value,
            }),
          });
          toast("Request sent");
          select(created.id);
        } catch (err) {
          error.textContent = err.message;
        } finally {
          send.disabled = false;
        }
      },
    }, [
      elem("label", { text: "Method and URL" }),
      elem("div", { class: "composer-row" }, [method, url]),
      elem("label", { text: "Headers" }),
      headersField,
      elem("label", { text: "Body" }),
      bodyField,
      d && d.request.truncated && cachedBody(d, "request") === null
        ? elem("p", { class: "body-info", text: "The original body was stored on disk and is not pre-filled here." })
        : null,
      error,
      elem("div", { class: "composer-row" }, [send]),
    ]);
    return form;
  };

  const openComposer = () => {
    state.selected = null;
    state.detail = null;
    state.tab = "edit";
    el.layout.classList.add("split");
    el.pane.hidden = false;
    renderList();
    el.detail.replaceChildren(elem("div", { class: "detail" }, [
      elem("div", { class: "detail-head" }, [
        elem("div", { class: "detail-title" }, [
          elem("span", { class: "method", text: "NEW" }),
          elem("button", { class: "icon-btn", type: "button", text: "×", onclick: closeDetail }),
        ]),
      ]),
      elem("div", { class: "tab-body" }, [renderComposer(null)]),
    ]));
  };

  // --- curl export ---

  const shellQuote = (s) => "'" + String(s).replace(/'/g, "'\\''") + "'";

  const toCurl = (d) => {
    const parts = ["curl -i -X " + d.method + " " + shellQuote(d.url)];
    for (const [name, value] of Object.entries(d.request_headers || {})) {
      if (name.toLowerCase() === "content-length") continue;
      parts.push("  -H " + shellQuote(name + ": " + value));
    }
    if (d.request.body) parts.push("  --data-raw " + shellQuote(d.request.body));
    if (d.request.truncated) parts.push("  # body truncated: fetch " + d.request.url + " for the full payload");
    return parts.join(" \\\n");
  };

  // --- Event stream ---

  const connect = () => {
    const source = new EventSource("/api/events");

    source.addEventListener("open", () => {
      el.connection.textContent = "live";
      el.connection.className = "pill pill-live";
    });

    source.addEventListener("error", () => {
      el.connection.textContent = "reconnecting";
      el.connection.className = "pill pill-down";
    });

    source.addEventListener("created", (e) => {
      upsertRow(JSON.parse(e.data));
      renderList();
    });

    source.addEventListener("updated", (e) => {
      const update = JSON.parse(e.data);
      if (!state.rows.has(update.id)) return;
      upsertRow(update);
      renderList();
      // A finished entry has metrics and a complete body worth re-fetching.
      if (state.selected === update.id && update.status !== "pending") {
        api("/api/requests/" + update.id).then((detail) => {
          state.detail = detail;
          state.live = { id: update.id, chunks: [] };
          renderDetail();
        }).catch(() => {});
      }
    });

    source.addEventListener("delta", (e) => {
      const delta = JSON.parse(e.data);
      if (state.selected !== delta.id) return;
      if (state.live.id !== delta.id) state.live = { id: delta.id, chunks: [] };
      state.live.chunks.push(delta.chunk);
      if (state.tab === "response" || state.tab === "ollama") {
        const box = document.querySelector(".tab-body");
        if (!box) return;
        // The tab body is the scroller now, so re-rendering it on every
        // fragment would throw the reader back to the top. Stay where the
        // reader is, unless they are already following the tail.
        const previous = box.scrollTop;
        const following = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
        box.replaceChildren(renderTab(state.detail));
        box.scrollTop = following ? box.scrollHeight : previous;
      }
    });

    source.addEventListener("deleted", (e) => {
      removeRow(JSON.parse(e.data).id);
      renderList();
    });
  };

  // --- Bootstrap ---

  const load = async () => {
    const data = await api("/api/requests?limit=200");
    const rows = data.requests || [];
    state.rows.clear();
    for (const row of rows) state.rows.set(row.id, row);
    // The API already returns entries newest first, which is the display order.
    // Going through upsertRow here would prepend each one and reverse the list.
    state.order = rows.map((row) => row.id);
    renderList();
  };

  const routeFromLocation = () => {
    const match = location.pathname.match(/^\/r\/([A-Za-z0-9]+)$/);
    if (match) select(match[1], true);
    else closeDetailSilently();
  };

  const closeDetailSilently = () => {
    state.selected = null;
    state.detail = null;
    el.pane.hidden = true;
    el.layout.classList.remove("split");
    renderList();
  };

  el.filter.addEventListener("input", (e) => {
    state.filter = e.target.value.trim().toLowerCase();
    renderList();
  });

  document.getElementById("compose").addEventListener("click", openComposer);

  document.getElementById("clear").addEventListener("click", async () => {
    if (!confirm("Delete the whole capture history?")) return;
    await api("/api/requests", { method: "DELETE" });
    state.rows.clear();
    state.order = [];
    state.bodyCache.clear();
    closeDetailSilently();
    history.replaceState({}, "", "/");
    toast("History cleared");
  });

  window.addEventListener("popstate", routeFromLocation);

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !el.pane.hidden) closeDetail();
  });

  // --- Resizable split ---

  const SPLIT_KEY = "http-debug:list-width";
  const MIN_SPLIT = 22;
  const MAX_SPLIT = 78;

  const applySplit = (percent) => {
    const clamped = Math.min(MAX_SPLIT, Math.max(MIN_SPLIT, percent));
    el.layout.style.setProperty("--list-width", clamped.toFixed(2) + "%");
    el.resizer.setAttribute("aria-valuenow", String(Math.round(clamped)));
    return clamped;
  };

  const storedSplit = () => {
    try {
      return Number(localStorage.getItem(SPLIT_KEY)) || 50;
    } catch (_) {
      return 50; // storage can be unavailable, the default still works
    }
  };

  let splitPercent = applySplit(storedSplit());

  const rememberSplit = () => {
    try {
      localStorage.setItem(SPLIT_KEY, String(splitPercent));
    } catch (_) { /* nothing to do, the width just will not persist */ }
  };

  el.resizer.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    el.resizer.setPointerCapture(e.pointerId);
    document.body.classList.add("resizing");
  });

  el.resizer.addEventListener("pointermove", (e) => {
    if (!el.resizer.hasPointerCapture(e.pointerId)) return;
    const bounds = el.layout.getBoundingClientRect();
    if (!bounds.width) return;
    splitPercent = applySplit(((e.clientX - bounds.left) / bounds.width) * 100);
  });

  const endResize = (e) => {
    if (!el.resizer.hasPointerCapture(e.pointerId)) return;
    el.resizer.releasePointerCapture(e.pointerId);
    document.body.classList.remove("resizing");
    rememberSplit();
  };

  el.resizer.addEventListener("pointerup", endResize);
  el.resizer.addEventListener("pointercancel", endResize);
  el.resizer.addEventListener("dblclick", () => {
    splitPercent = applySplit(50);
    rememberSplit();
  });

  el.resizer.addEventListener("keydown", (e) => {
    const step = e.shiftKey ? 10 : 2;
    if (e.key === "ArrowLeft") splitPercent = applySplit(splitPercent - step);
    else if (e.key === "ArrowRight") splitPercent = applySplit(splitPercent + step);
    else if (e.key === "Home") splitPercent = applySplit(50);
    else return;
    e.preventDefault();
    rememberSplit();
  });

  setInterval(tickElapsed, 100);

  api("/api/config")
    .then((cfg) => { el.target.textContent = "→ " + cfg.target_url; })
    .catch(() => {});

  load().then(routeFromLocation).catch((err) => {
    el.empty.textContent = "Could not load requests: " + err.message;
    el.empty.hidden = false;
  });

  connect();
})();
