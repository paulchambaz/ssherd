// ─── Utilities ───────────────────────────────────────────────────────────────

function onReady(fn) {
  if (document.readyState !== "loading") fn();
  else document.addEventListener("DOMContentLoaded", fn);
}

function esc(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// ─── Theme ───────────────────────────────────────────────────────────────────

onReady(() => {
  const theme = localStorage.getItem("theme");
  if (theme === "dark") document.documentElement.classList.add("dark");
  else if (theme === "light") document.documentElement.classList.remove("dark");
  else {
    const darkMode = window.matchMedia("(prefers-color-scheme: dark)");
    const update = (b) => {
      if (localStorage.getItem("theme") !== null) return;
      if (b) document.documentElement.classList.add("dark");
      else document.documentElement.classList.remove("dark");
    };
    update(darkMode.matches);
    darkMode.addEventListener("change", (e) => update(e.matches));
  }
});

function toggleTheme() {
  const isDarkMode = document.documentElement.classList.contains("dark");
  localStorage.setItem("theme", isDarkMode ? "light" : "dark");
  document.documentElement.classList.toggle("dark");
}

// ─── Console tabs (job detail page) ──────────────────────────────────────────

onReady(() => {
  const tabStdout = document.getElementById("tab-stdout");
  const tabStderr = document.getElementById("tab-stderr");
  if (!tabStdout || !tabStderr) return;

  const active =
    "px-3 py-1.5 text-xs font-semibold rounded-md bg-base-200 text-base-700";
  const inactive =
    "px-3 py-1.5 text-xs font-semibold rounded-md text-base-400 hover:text-base-600";

  function switchTab(tab) {
    const jobId = document.querySelector("[data-job-id]")?.dataset.jobId ?? "";
    const stdout = document.getElementById("console-stdout-" + jobId);
    const stderr = document.getElementById("console-stderr-" + jobId);
    const btnRaw = document.getElementById("btn-raw");
    if (tab === "stdout") {
      stdout.classList.remove("hidden");
      stderr.classList.add("hidden");
      tabStdout.className = active;
      tabStderr.className = inactive;
      if (btnRaw)
        btnRaw.href = btnRaw.href.replace(/\/(stdout|stderr)$/, "/stdout");
      stdout.scrollTop = stdout.scrollHeight;
    } else {
      stderr.classList.remove("hidden");
      stdout.classList.add("hidden");
      tabStderr.className = active;
      tabStdout.className = inactive;
      if (btnRaw)
        btnRaw.href = btnRaw.href.replace(/\/(stdout|stderr)$/, "/stderr");
      stderr.scrollTop = stderr.scrollHeight;
    }
  }

  tabStdout.addEventListener("click", () => switchTab("stdout"));
  tabStderr.addEventListener("click", () => switchTab("stderr"));

  const el = document.getElementById("console-stdout");
  if (el) el.scrollTop = el.scrollHeight;
});

// ─── Batch form (new batch page) ─────────────────────────────────────────────

onReady(() => {
  const form = document.getElementById("batch-form");
  if (!form) return;

  let nextIdx = 0;
  let previewExpanded = false;

  function getAxes() {
    const axes = [];
    document.querySelectorAll(".axis-block").forEach((block) => {
      const idx = block.dataset.idx;
      const name =
        (document.getElementById("axis_" + idx + "_name") || {}).value || "";
      const raw =
        (document.getElementById("axis_" + idx + "_values") || {}).value || "";
      const values = raw
        .split("\n")
        .map((v) => v.trim())
        .filter((v) => v);
      if (values.length > 0) axes.push({ name: name.trim(), values });
    });
    return axes;
  }

  function cartesian(axes) {
    if (axes.length === 0) return [[]];
    const [first, ...rest] = axes;
    const sub = cartesian(rest);
    return first.values.flatMap((v) => sub.map((r) => [v, ...r]));
  }

  function updatePreview() {
    const base = (document.getElementById("base_command") || {}).value || "";
    const seedFlag =
      (document.getElementById("seed_flag") || {}).value || "--seed";
    const numSeeds =
      parseInt((document.getElementById("num_seeds") || {}).value) || 1;
    const axes = getAxes();
    const combos = cartesian(axes);
    const total = combos.length * numSeeds;

    const countEl = document.getElementById("preview_count");
    if (countEl)
      countEl.textContent = total + " job" + (total !== 1 ? "s" : "");

    const toggleBtn = document.getElementById("preview_toggle");
    if (toggleBtn) {
      toggleBtn.textContent = previewExpanded ? "Show less" : "Show all";
      toggleBtn.style.display = total <= 3 ? "none" : "";
    }

    const allLines = [];
    for (const combo of combos) {
      for (let s = 1; s <= numSeeds; s++) {
        const cmd = [base, ...combo, seedFlag, s]
          .filter((p) => p !== "")
          .join(" ");
        allLines.push('<div class="truncate">' + esc(cmd) + "</div>");
      }
    }

    const el = document.getElementById("preview_commands");
    if (!el) return;

    if (allLines.length === 0) {
      el.innerHTML =
        '<div class="text-base-400">Enter a base command to preview.</div>';
      return;
    }

    if (previewExpanded) {
      el.innerHTML = allLines.join("");
    } else {
      const visible = allLines.slice(0, 3).join("");
      const more =
        total > 3
          ? '<div class="text-base-400 mt-1">… and ' +
          (total - 3) +
          ' more — click "Show all" to expand</div>'
          : "";
      el.innerHTML = visible + more;
    }

    const hidden = document.getElementById("axes_json");
    if (hidden) hidden.value = JSON.stringify(getAxes());
  }

  window.togglePreview = function() {
    previewExpanded = !previewExpanded;
    updatePreview();
  };

  function addAxis() {
    const container = document.getElementById("axes_container");
    const placeholder = container.querySelector("p");
    if (placeholder) placeholder.remove();

    const idx = nextIdx++;
    const div = document.createElement("div");
    div.className =
      "axis-block border border-base-300 rounded-md overflow-hidden";
    div.dataset.idx = idx;
    div.innerHTML =
      '<div class="flex items-center gap-2 px-3 py-2 bg-base-50 border-b border-base-300">' +
      '<input id="axis_' +
      idx +
      '_name" type="text" placeholder="Axis name" ' +
      'class="bg-transparent text-sm font-semibold focus:outline-none flex-1 placeholder:font-normal placeholder:text-base-400" />' +
      '<button type="button" class="remove-axis text-xs text-red-400 hover:text-red-600 shrink-0">Remove</button>' +
      "</div>" +
      '<textarea id="axis_' +
      idx +
      '_values" rows="3" ' +
      'placeholder="--env antmaze-large-play-v2\n--env antmaze-large-diverse-v2" ' +
      'class="w-full bg-transparent text-sm font-mono px-3 py-2.5 focus:outline-none resize-y placeholder:text-base-400"></textarea>';

    div.querySelector("input").addEventListener("input", updatePreview);
    div.querySelector("textarea").addEventListener("input", updatePreview);
    div
      .querySelector(".remove-axis")
      .addEventListener("click", () => removeAxis(div));
    container.appendChild(div);
    updatePreview();
  }

  function removeAxis(block) {
    block.remove();
    const container = document.getElementById("axes_container");
    if (container.querySelectorAll(".axis-block").length === 0) {
      const p = document.createElement("p");
      p.className = "text-sm text-base-400 py-2";
      p.textContent =
        "No axes yet — jobs will be created from the base command alone.";
      container.appendChild(p);
    }
    updatePreview();
  }

  ["base_command", "seed_flag", "num_seeds"].forEach((id) => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("input", updatePreview);
  });

  form.addEventListener("submit", () => {
    document.getElementById("axes_json").value = JSON.stringify(getAxes());
  });

  window.addAxis = addAxis;
  updatePreview();
});

// ─── Viz form (new visualization page) ───────────────────────────────────────

window.toggleVizMode = function() {
  const toggle = document.getElementById("viz_mode_toggle");
  const thumb = document.getElementById("viz_mode_thumb");
  const hidden = document.getElementById("viz_build_remote");
  const labelLocal = document.getElementById("viz_mode_label_local");
  const labelRemote = document.getElementById("viz_mode_label_remote");
  if (!toggle) return;

  const isRemote = toggle.getAttribute("aria-checked") === "true";
  const next = !isRemote;

  toggle.setAttribute("aria-checked", next ? "true" : "false");
  toggle.classList.toggle("bg-accent-500", next);
  toggle.classList.toggle("bg-base-200", !next);
  thumb.classList.toggle("translate-x-4", next);
  thumb.classList.toggle("translate-x-0", !next);
  labelLocal.className =
    "text-sm font-medium " + (next ? "text-base-400" : "text-base-700");
  labelRemote.className =
    "text-sm font-medium " + (next ? "text-base-700" : "text-base-400");
  if (hidden) hidden.value = next ? "true" : "false";
};

onReady(() => {
  const form = document.getElementById("viz-form");
  if (!form) return;

  let nextVizIdx = 0;

  function slugify(s) {
    return s
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-|-$/g, "");
  }

  function lastToken(s) {
    const parts = s.trim().split(/\s+/);
    return parts[parts.length - 1] || s;
  }

  function getVizAxes() {
    const axes = [];
    document.querySelectorAll(".viz-axis-block").forEach((block) => {
      const idx = block.dataset.idx;
      const name =
        (document.getElementById("viz_axis_" + idx + "_name") || {}).value ||
        "";
      const raw =
        (document.getElementById("viz_axis_" + idx + "_values") || {}).value ||
        "";
      const values = raw
        .split("\n")
        .map((v) => v.trim())
        .filter((v) => v);
      if (values.length > 0) axes.push({ name: name.trim(), values });
    });
    return axes;
  }

  function cartesianViz(arrays) {
    if (arrays.length === 0) return [[]];
    const [first, ...rest] = arrays;
    const sub = cartesianViz(rest);
    return first.flatMap((v) => sub.map((r) => [v, ...r]));
  }

  function resolveCmd(cmdTpl, combo) {
    const versionParts = combo.map(({ val }) => slugify(lastToken(val)));
    const version =
      versionParts.length > 0 ? versionParts.join("_") : "default";
    let cmd = cmdTpl;
    cmd = cmd.replaceAll("{version}", version);
    combo.forEach(({ axis, val }) => {
      cmd = cmd.replaceAll("{" + axis.name + "}", lastToken(val));
    });
    return cmd;
  }

  function updateVizPreview() {
    const cmdTpl =
      (document.getElementById("viz_command_input") || {}).value || "";
    const axes = getVizAxes();
    const toggleable = axes.filter((ax) => ax.toggleable);
    const fixed = axes.filter((ax) => !ax.toggleable);

    let count = 1;
    toggleable.forEach((ax) => {
      count *= ax.values.length || 1;
    });

    const countEl = document.getElementById("viz_preview_count");
    if (countEl)
      countEl.textContent = count + " output" + (count !== 1 ? "s" : "");

    const combos = cartesianViz(
      toggleable.map((ax) => ax.values.map((v) => ({ axis: ax, val: v }))),
    );

    const lines = combos.map((combo) => {
      const resolvedBase = resolveCmd(cmdTpl, combo);
      const axisArgs = combo.map(({ val }) => val).filter(Boolean);
      const fullCmd =
        axisArgs.length > 0
          ? resolvedBase + " " + axisArgs.join(" ")
          : resolvedBase;
      return '<div class="truncate">' + esc(fullCmd) + "</div>";
    });

    const el = document.getElementById("viz_preview_commands");
    if (!el) return;
    el.innerHTML =
      lines.length > 0 && cmdTpl
        ? lines.join("")
        : '<div class="text-base-400">Fill in the viz command to see a preview.</div>';

    const hidden = document.getElementById("axes_json");
    if (hidden) hidden.value = JSON.stringify(axes);
  }

  window.addVizAxis = function() {
    const container = document.getElementById("viz_axes_container");
    const empty = document.getElementById("viz_axes_empty");
    if (empty) empty.remove();

    const idx = nextVizIdx++;
    const div = document.createElement("div");
    div.className =
      "viz-axis-block border border-base-300 rounded-md overflow-hidden";
    div.dataset.idx = idx;
    div.innerHTML = `
    <div class="flex items-center gap-2 px-3 py-2 bg-base-50 border-b border-base-300">
      <input id="viz_axis_${idx}_name" type="text" placeholder="name"
        class="bg-transparent text-sm font-semibold focus:outline-none w-28 placeholder:font-normal placeholder:text-base-400" />
      <button type="button" class="remove-viz-axis text-xs text-red-400 hover:text-red-600 shrink-0 ml-auto">Remove</button>
    </div>
    <textarea id="viz_axis_${idx}_values" rows="3"
      placeholder="--env sine\n--env cosine\n--env square"
      class="w-full bg-transparent text-sm font-mono px-3 py-2.5 focus:outline-none resize-y placeholder:text-base-400"></textarea>
  `;

    div
      .querySelector("#viz_axis_" + idx + "_name")
      .addEventListener("input", updateVizPreview);
    div
      .querySelector("#viz_axis_" + idx + "_values")
      .addEventListener("input", updateVizPreview);
    div.querySelector(".remove-viz-axis").addEventListener("click", () => {
      div.remove();
      if (!document.querySelector(".viz-axis-block")) {
        const p = document.createElement("p");
        p.id = "viz_axes_empty";
        p.className = "text-sm text-base-400 py-2";
        p.textContent = "No axes yet — the command will be called as-is.";
        document.getElementById("viz_axes_container").appendChild(p);
      }
      updateVizPreview();
    });

    container.appendChild(div);
    updateVizPreview();
  };

  document
    .getElementById("viz_command_input")
    ?.addEventListener("input", updateVizPreview);
  document
    .getElementById("viz_output_tpl")
    ?.addEventListener("input", updateVizPreview);

  form.addEventListener("submit", () => {
    const hidden = document.getElementById("axes_json");
    if (hidden) hidden.value = JSON.stringify(getVizAxes());
  });

  updateVizPreview();
});

// ─── Viz viewer (page detail visualisation) ──────────────────────────────────
onReady(() => {
  const viewer = document.getElementById("viz-viewer");
  if (!viewer) return;

  const axes = JSON.parse(viewer.dataset.axes || "[]");
  const fileBaseUrl = viewer.dataset.fileUrl;

  const controls = document.getElementById("viz-controls");
  const img = document.getElementById("viz-output-img");
  const placeholder = document.getElementById("viz-output-placeholder");
  const errorEl = document.getElementById("viz-output-error");
  const downloadLink = document.getElementById("viz-download-link");
  const label = document.getElementById("viz-output-label");

  // Capture whether the server rendered a gen error before JS touches the DOM
  const hadServerError = !errorEl.classList.contains("hidden");

  // Build selection state from axes defaults
  const selection = {};
  axes.forEach((ax) => {
    if (ax.values && ax.values.length > 0) selection[ax.name] = ax.values[0];
  });

  // Build axis toggle controls
  if (axes.length > 0) {
    controls.classList.remove("hidden");
    axes.forEach((ax) => {
      const row = document.createElement("div");
      row.className = "flex items-center justify-center gap-2 flex-wrap";

      if (ax.name) {
        const lbl = document.createElement("span");
        lbl.className = "text-xs font-semibold text-base-500 w-24 shrink-0 text-right";
        lbl.textContent = ax.name;
        row.appendChild(lbl);
      }

      ax.values.forEach((val) => {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.dataset.axis = ax.name;
        btn.dataset.value = val;
        btn.textContent = val;
        btn.addEventListener("click", () => {
          selection[ax.name] = val;
          updateButtons();
          loadOutput();
        });
        row.appendChild(btn);
      });

      if (ax.name) {
        const spacer = document.createElement("span");
        spacer.className = "w-24 shrink-0";
        row.appendChild(spacer);
      }

      controls.appendChild(row);
    });
  }

  function updateButtons() {
    controls.querySelectorAll("button[data-axis]").forEach((btn) => {
      const active = selection[btn.dataset.axis] === btn.dataset.value;
      btn.className = active
        ? "text-xs px-2.5 py-1 rounded border bg-accent-500 text-white border-accent-600 transition-colors"
        : "text-xs px-2.5 py-1 rounded border bg-base-50 text-base-600 border-base-300 hover:bg-base-100 transition-colors";
    });
  }

  function buildUrl() {
    const params = new URLSearchParams();
    Object.entries(selection).forEach(([k, v]) => params.set(k, v));
    const qs = params.toString();
    return fileBaseUrl + (qs ? "?" + qs : "");
  }

  function loadOutput() {
    const url = buildUrl();
    img.classList.add("hidden");
    placeholder.classList.add("hidden");
    if (!hadServerError) errorEl.classList.add("hidden");
    if (label) label.textContent = "";
    if (downloadLink) downloadLink.classList.add("hidden");

    fetch(url, { method: "HEAD" })
      .then((res) => {
        if (!res.ok) {
          if (!hadServerError) placeholder.classList.remove("hidden");
          return;
        }
        // Successful — clear any server error and show the image
        errorEl.classList.add("hidden");
        img.src = url + (url.includes("?") ? "&" : "?") + "_t=" + Date.now();
        img.onload = () => img.classList.remove("hidden");
        img.onerror = () => {
          img.classList.add("hidden");
          errorEl.classList.remove("hidden");
        };
        if (label) {
          const parts = Object.entries(selection).map(([k, v]) => k + "=" + v);
          label.textContent = parts.length > 0 ? parts.join(" · ") : "default";
        }
        if (downloadLink) {
          downloadLink.href = url;
          downloadLink.classList.remove("hidden");
        }
      })
      .catch(() => {
        errorEl.classList.remove("hidden");
      });
  }

  updateButtons();
  loadOutput();
});
