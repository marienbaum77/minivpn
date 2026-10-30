"use strict";

const $ = (id) => document.getElementById(id);
const num = (v, d = 2) => (v || 0).toLocaleString("ru-RU", { minimumFractionDigits: d, maximumFractionDigits: d });
const rub = (v) => num(v) + " ₽";
const SVG = "http://www.w3.org/2000/svg";

// 5x7 dot-matrix glyphs, one string per row.
const GLYPHS = {
  0: ["01110", "10001", "10011", "10101", "11001", "10001", "01110"],
  1: ["00100", "01100", "00100", "00100", "00100", "00100", "01110"],
  2: ["01110", "10001", "00001", "00010", "00100", "01000", "11111"],
  3: ["11111", "00010", "00100", "00010", "00001", "10001", "01110"],
  4: ["00010", "00110", "01010", "10010", "11111", "00010", "00010"],
  5: ["11111", "10000", "11110", "00001", "00001", "10001", "01110"],
  6: ["00110", "01000", "10000", "11110", "10001", "10001", "01110"],
  7: ["11111", "00001", "00010", "00100", "01000", "01000", "01000"],
  8: ["01110", "10001", "10001", "01110", "10001", "10001", "01110"],
  9: ["01110", "10001", "10001", "01111", "00001", "00010", "01100"],
  ":": ["0", "0", "1", "0", "1", "0", "0"],
  ",": ["00", "00", "00", "00", "01", "01", "10"],
  "₽": ["01110", "01001", "01001", "01110", "11100", "01000", "01000"],
  " ": ["0", "0", "0", "0", "0", "0", "0"],
};

// matrix draws text as lit dots over a faint grid, Nothing-style. The dot
// pitch shrinks when the text would not fit, e.g. a timer past 9:59:59.
function matrix(el, text, maxPitch, idle) {
  const glyphs = [...text].map((c) => GLYPHS[c] || GLYPHS[" "]);
  const cols = glyphs.reduce((n, g) => n + g[0].length + 1, -1);
  const pitch = Math.min(maxPitch, Math.floor(el.clientWidth / cols) || maxPitch);
  const r = pitch * 0.38;
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("width", cols * pitch);
  svg.setAttribute("height", 7 * pitch);
  let x = 0;
  for (const g of glyphs) {
    for (let row = 0; row < 7; row++) {
      for (let col = 0; col < g[row].length; col++) {
        const c = document.createElementNS(SVG, "circle");
        c.setAttribute("cx", (x + col + 0.5) * pitch);
        c.setAttribute("cy", (row + 0.5) * pitch);
        c.setAttribute("r", r);
        c.setAttribute("class", g[row][col] === "1" ? "on" : "off");
        svg.appendChild(c);
      }
    }
    x += g[0].length + 1;
  }
  el.classList.toggle("idle", !!idle);
  el.replaceChildren(svg);
}

// Startup is the slow part, so it shows where it is: server, boot and SSH
// login, the setup script's step, tunnel.
function step(st) {
  switch (st.phase) {
    case "creating": return [0, "сервер"];
    case "installing":
      if (st.node_stage === "setup") return [2, st.node_step || "sing-box"];
      if (st.node_stage === "ready") return [2, "проверка"];
      return [1, st.node_step || "загрузка"];
    case "connecting": return [3, "туннель"];
  }
  return null;
}
const PHASES = { off: "выкл", creating: "старт", installing: "старт", connecting: "старт", on: "в сети", paused: "пауза", stopping: "удаляю" };
const REASONS = { stop: "стоп", exit: "выход", limit: "лимит", "node-gone": "простой", failed: "сбой", tunnel: "sing-box", recovered: "после сбоя" };
const STAGES = { tcp: "нет tcp", handshake: "reality режут", bulk: "тспу" };

async function api(path, body) {
  const res = await fetch("api/" + path, body === undefined ? {} : {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || res.statusText);
    err.data = data;
    throw err;
  }
  return data;
}

function hms(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(s / 3600);
  const m = String(Math.floor((s % 3600) / 60)).padStart(2, "0");
  return `${h}:${m}:${String(s % 60).padStart(2, "0")}`;
}
for (let i = 0; i < 12; i++) {
  const a = (i / 12) * 2 * Math.PI - Math.PI / 2;
  const c = document.createElementNS(SVG, "circle");
  c.setAttribute("cx", 50 + 38 * Math.cos(a));
  c.setAttribute("cy", 50 + 38 * Math.sin(a));
  c.setAttribute("r", 3.5);
  c.style.animationDelay = `${(i / 12) * 1.2 - 1.2}s`;
  $("spin").appendChild(c);
}

let localError = "";
let lastLink = "";
let lastText = {};

// draw redraws a matrix only when its text changed.
function draw(id, text, pitch, idle) {
  const key = text + (idle ? "~" : "");
  if (lastText[id] === key) return;
  lastText[id] = key;
  matrix($(id), text, pitch, idle);
}

window.addEventListener("resize", () => { lastText = {}; });

function render({ status: st, now, recent }) {
  const running = st.phase !== "off";
  const busy = running && st.phase !== "on";
  const err = localError || st.error || "";

  $("rec").className = "rec " + (st.phase === "on" ? "on" : busy ? "busy" : "");
  $("phase").textContent = PHASES[st.phase] || st.phase;
  $("rate").textContent = running && st.charged ? `списано ${num(st.charged_rub)}` : st.rate_per_hour ? `${num(st.rate_per_hour)} ₽/ч` : "";

  const started = new Date(st.started_at).getTime();
  draw("timer", running && started > 0 ? hms(new Date(now).getTime() - started) : "0:00:00", 9, !running);

  const s = step(st);
  $("steps").classList.toggle("hidden", !s);
  if (s) {
    [...$("steps").querySelectorAll("i")].forEach((d, i) => { d.className = i < s[0] ? "done" : i === s[0] ? "now" : ""; });
    $("step").textContent = s[1];
  }
  // While starting, the newest log line says what exactly is going on.
  $("last-line").classList.toggle("hidden", !s || !lastLog);
  $("last-line").textContent = lastLog.replace(/^\d\d:\d\d:\d\d /, "");
  $("ask").classList.toggle("hidden", !st.ask_extend);
  if (st.ask_extend) {
    $("ask-text").textContent = "до " + new Date(st.deadline).toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" });
  }

  const power = $("power");
  power.className = "tile power " + (st.phase === "on" ? "on" : busy ? "busy" : "");
  power.disabled = st.phase === "stopping";
  power.setAttribute("aria-label", running ? "стоп" : "пуск");
  $("power-label").textContent = running ? "стоп" : "пуск";
  $("spin").classList.toggle("hidden", !busy);

  const pause = $("pause");
  if (pause) {
    const isPaused = st.phase === "paused";
    const canPause = st.phase === "on";
    pause.disabled = !isPaused && !canPause;
    pause.className = "tile pause" + (isPaused ? " paused" : "");
    pause.setAttribute("aria-label", isPaused ? "продолжить" : "пауза");
    $("pause-label").textContent = isPaused ? "продолжить" : "пауза";
  }

  $("alert").classList.toggle("hidden", !err);
  $("error").textContent = err;

  const spent = (st.month_spent || 0) + (running ? st.estimated_rub || 0 : 0);
  $("balance").textContent = st.balance ? "баланс " + num(st.balance, 0) : "";
  $("balance").style.color = st.balance && st.balance < 100 ? "var(--red)" : "";
  $("total").textContent = rub(spent);

  $("no-node").classList.toggle("hidden", !!st.node_ip);
  $("node-info").classList.toggle("hidden", !st.node_ip);
  $("node-ip").textContent = st.node_ip || "";
  const c = st.last_check;
  $("last-check").className = c && !c.OK ? "bad" : "";
  $("last-check").textContent = !c ? "—" : c.OK
    ? `${Math.round(c.Handshake / 1e6)} мс` + (c.SpeedMbps ? ` · ${num(c.SpeedMbps, 1)} мбит` : "")
    : STAGES[c.Failed] || c.Error;
  const r = st.reputation;
  if (!r) {
    $("reputation").textContent = "—";
    $("reputation").className = "";
  } else {
    const bad = [];
    if (r.country === "RU" || !r.country) bad.push("страна");
    if (r.proxy) bad.push("vpn-база");
    if (!r.reddit_ok) bad.push("reddit");
    if (!r.openai_ok) bad.push("openai");
    if (r.google_captcha) bad.push("капча");
    $("reputation").textContent = (r.country || "?") + " · " + (bad.length ? bad.join(" ") : "чисто");
    $("reputation").className = bad.length ? "bad" : "";
  }
  if (st.link && st.link !== lastLink) $("qr").src = "api/qr.png?t=" + Date.now();
  lastLink = st.link || "";

  const box = $("recent");
  box.replaceChildren();
  if (!recent || !recent.length) {
    const p = document.createElement("p");
    p.className = "empty";
    p.textContent = "пусто";
    box.appendChild(p);
  }
  for (const t of recent || []) {
    const row = document.createElement("div");
    row.className = "trip";
    const when = document.createElement("span");
    when.textContent = new Date(t.started_at).toLocaleString("ru-RU", { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" }).replace(",", "");
    const dur = document.createElement("span");
    dur.className = "dur";
    dur.textContent = t.minutes + "м";
    const sum = document.createElement("span");
    sum.className = "sum";
    sum.textContent = num(t.rub);
    const meta = document.createElement("small");
    meta.textContent = [REASONS[t.reason] || t.reason, t.isp].filter(Boolean).join(" · ");
    row.append(when, dur, sum, meta);
    box.appendChild(row);
  }
}

let logNext = 0;
let lastLog = "";

// pullLog appends the lines logged since the last call. Warnings and errors
// ("!") stand out in red.
async function pullLog() {
  const r = await api("log?after=" + logNext);
  logNext = r.next;
  if (!r.lines || !r.lines.length) return;
  lastLog = r.lines[r.lines.length - 1];
  const pre = $("log-lines");
  const sheet = $("log");
  const atEnd = sheet.scrollTop + sheet.clientHeight >= sheet.scrollHeight - 8;
  for (const line of r.lines) {
    const el = document.createElement(line.includes(" ! ") || /failed at|упала|не удалось/.test(line) ? "b" : "span");
    el.textContent = line + "\n";
    pre.appendChild(el);
  }
  while (pre.childNodes.length > 300) pre.firstChild.remove();
  if (atEnd) sheet.scrollTop = sheet.scrollHeight;
}

async function tick() {
  pullLog().catch(() => {});
  try {
    render(await api("state"));
  } catch (e) {
    $("phase").textContent = "нет связи";
  }
}

$("power").onclick = () => {
  if ($("power").classList.contains("on") || $("power").classList.contains("busy") || $("power-label").textContent === "стоп") {
    localError = "";
    api("stop", {}).finally(tick);
  } else {
    start(false);
  }
};
$("pause").onclick = () => {
  const p = $("pause");
  if (p.disabled) return;
  p.disabled = true;
  const isPaused = p.classList.contains("paused");
  api(isPaused ? "resume" : "pause", {}).finally(tick);
};
$("force").onclick = () => start(true);

async function start(force) {
  localError = "";
  $("force").classList.add("hidden");
  $("power").disabled = true;
  try {
    await api("start", { force });
  } catch (e) {
    localError = e.message;
    if (e.data && e.data.limit) $("force").classList.remove("hidden");
  } finally {
    $("power").disabled = false;
    tick();
  }
}

$("extend").onclick = () => api("extend", {}).finally(tick);
$("pin").onclick = async () => {
  const on = $("pin").getAttribute("aria-pressed") !== "true";
  $("pin").setAttribute("aria-pressed", String(on));
  await api("pin", { pinned: on });
};

for (const b of document.querySelectorAll("[data-sheet]")) {
  b.onclick = () => {
    const sheet = $(b.dataset.sheet);
    sheet.classList.remove("hidden");
    if (sheet.id === "log") sheet.scrollTop = sheet.scrollHeight;
  };
}
for (const b of document.querySelectorAll(".close")) {
  b.onclick = () => b.closest(".sheet").classList.add("hidden");
}
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") document.querySelectorAll(".sheet").forEach((s) => s.classList.add("hidden"));
});

$("export").onclick = async () => {
  try {
    const r = await api("export", {});
    $("export-path").textContent = r.path;
  } catch (e) {
    $("export-path").textContent = e.message;
  }
};

const form = $("settings-form");
const lines = (s) => s.split(/\r?\n/).map((x) => x.trim()).filter(Boolean);

async function loadSettings() {
  const s = await api("settings");
  const firstRun = !s.has_key;
  form.api_key.placeholder = s.has_key ? "••••••••" : "вставь ключ";
  form.api_key.required = firstRun;
  for (const k of ["monthly_limit", "warn_at", "session_hours", "node_max_hours", "idle_minutes", "idle_mb", "location", "sni"]) {
    form[k].value = s[k] ?? "";
  }
  form.game_dirs.value = (s.game_dirs || []).join("\n");
  form.direct_exes.value = (s.direct_exes || []).join("\n");
  form.autostart.checked = s.autostart;
  const found = s.detected || [];
  $("detected").textContent = found.length ? `· +${found.length} найдено` : "";
  $("detected").title = found.join("\n");
  $("welcome").classList.toggle("hidden", !firstRun);
  $("settings-close").classList.toggle("hidden", firstRun);
  if (firstRun) {
    $("settings").classList.remove("hidden");
    form.api_key.focus();
  }
}

form.onsubmit = async (ev) => {
  ev.preventDefault();
  const n = (k) => Number(form[k].value);
  const msg = $("settings-msg");
  try {
    await api("settings", {
      api_key: form.api_key.value.trim(),
      monthly_limit: n("monthly_limit"),
      warn_at: n("warn_at"),
      session_hours: n("session_hours"),
      node_max_hours: n("node_max_hours"),
      idle_minutes: n("idle_minutes"),
      idle_mb: n("idle_mb"),
      location: form.location.value.trim(),
      sni: form.sni.value.trim(),
      game_dirs: lines(form.game_dirs.value),
      direct_exes: lines(form.direct_exes.value),
      autostart: form.autostart.checked,
    });
    form.api_key.value = "";
    msg.textContent = "записано";
    msg.className = "label wide";
    localError = "";
    loadSettings();
  } catch (e) {
    msg.textContent = e.message;
    msg.className = "label wide bad";
  }
};

loadSettings();
tick();
setInterval(tick, 1000);
