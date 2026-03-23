let allBatchLines = [];
let allVizLines = [];

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

  function sanitizeAxisValue(s) {
    return s
      .trim()
      .split(/\s+/)
      .join("_")
      .replace(/[^a-zA-Z0-9\-\_\.]+/g, "")
      .replace(/_+/g, "_")
      .replace(/^_|_$/g, "");
  }

  function substituteVars(s, vars) {
    let out = s;
    for (const [k, v] of Object.entries(vars)) {
      out = out.replaceAll("{" + k + "}", v);
    }
    return out;
  }

  function updatePreview() {
    const base = (document.getElementById("base_command") || {}).value || "";

    if (!base.trim()) {
      const el = document.getElementById("preview_commands");
      if (el)
        el.innerHTML =
          '<div class="text-base-400">Enter a base command to preview.</div>';
      const countEl = document.getElementById("preview_count");
      if (countEl) countEl.textContent = "";
      const toggleBtn = document.getElementById("preview_toggle");
      if (toggleBtn) toggleBtn.style.display = "none";
      return;
    }

    const seedFlag =
      (document.getElementById("seed_flag") || {}).value || "--seed";
    const startSeed =
      parseInt((document.getElementById("start_seed") || {}).value) || 1;
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
      toggleBtn.style.display = total <= 5 ? "none" : "";
      toggleBtn.textContent = previewExpanded ? "Show less" : "Show all";
    }

    const logArg =
      (form.querySelector('[name="log_argument"]') || {}).value || "";
    const outputArg =
      (form.querySelector('[name="output_argument"]') || {}).value || "";
    const logPathTplVal =
      (form.querySelector('[name="log_path"]') || {}).value || "";
    const outputPathTplVal =
      (form.querySelector('[name="output_path"]') || {}).value || "";
    const dataPath = form.dataset.dataPath || "";

    const allLines = [];
    for (const combo of combos) {
      const ablationParts = combo.map((v) => sanitizeAxisValue(v));
      const ablation =
        ablationParts.length > 0 ? ablationParts.join("_") : "run";

      for (let s = startSeed; s < startSeed + numSeeds; s++) {
        const vars = { seed: String(s), ablation };
        axes.forEach((ax, i) => {
          if (ax.name && i < combo.length) {
            vars[ax.name] = sanitizeAxisValue(combo[i]);
          }
        });

        const tokens = [base, ...combo, seedFlag, String(s)].filter(Boolean);
        let cmd = substituteVars(tokens.join(" "), vars);

        if (logArg && logPathTplVal) {
          cmd += " " + logArg + " <nfs_job_dir>/" + logPathTplVal;
        }
        if (outputArg && outputPathTplVal) {
          const resolvedOutput = substituteVars(outputPathTplVal, vars);
          const prefix = dataPath ? "{temporary_path}/" + dataPath + "/" : "";
          cmd += " " + outputArg + " " + prefix + resolvedOutput;
        }

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

    allBatchLines = allLines.map((l) => {
      const tmp = document.createElement("div");
      tmp.innerHTML = l;
      return [...tmp.querySelectorAll("div")]
        .map((d) => d.textContent.trim())
        .filter(Boolean)
        .join("\n");
    });

    if (previewExpanded) {
      el.innerHTML = allLines.join("");
    } else {
      const visible = allLines.slice(0, 5).join("");
      const more =
        total > 5
          ? '<div class="text-base-400 mt-1">… and ' +
          (total - 5) +
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

  function addAxis(initialName = "", initialValues = "") {
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

    const nameInput = div.querySelector("input");
    const valuesTextarea = div.querySelector("textarea");

    nameInput.addEventListener("input", updatePreview);
    valuesTextarea.addEventListener("input", updatePreview);
    div
      .querySelector(".remove-axis")
      .addEventListener("click", () => removeAxis(div));

    if (initialName) nameInput.value = initialName;
    if (initialValues) valuesTextarea.value = initialValues;

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

  ["base_command", "seed_flag", "start_seed", "num_seeds"].forEach((id) => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("input", updatePreview);
  });
  ["log_argument", "output_argument", "log_path", "output_path"].forEach(
    (name) => {
      const el = form.querySelector('[name="' + name + '"]');
      if (el) el.addEventListener("input", updatePreview);
    },
  );

  form.addEventListener("submit", () => {
    document.getElementById("axes_json").value = JSON.stringify(getAxes());
  });

  window.addAxis = addAxis;

  // ─── Redo : restauration du FormState ────────────────────────────────────
  const stateJSON = form.dataset.formState;
  if (stateJSON) {
    try {
      const state = JSON.parse(stateJSON);
      const setVal = (selector, val) => {
        if (val == null) return;
        const el = form.querySelector(selector);
        if (el) el.value = val;
      };

      setVal('[name="name_prefix"]', state.name_prefix);
      setVal('[name="log_path"]', state.log_path);
      setVal('[name="output_path"]', state.output_path);
      setVal('[name="output_files"]', state.output_files);
      setVal('[name="min_vram"]', state.min_vram);
      setVal('[name="max_retries"]', state.max_retries);
      setVal('[name="retry_suffix"]', state.retry_suffix);
      setVal('[name="log_argument"]', state.log_argument);
      setVal('[name="output_argument"]', state.output_argument);

      const baseCmd = document.getElementById("base_command");
      if (baseCmd && state.base_command != null)
        baseCmd.value = state.base_command;

      const seedFlagEl = document.getElementById("seed_flag");
      if (seedFlagEl && state.seed_flag != null)
        seedFlagEl.value = state.seed_flag;

      const startSeedEl = document.getElementById("start_seed");
      if (startSeedEl && state.start_seed != null)
        startSeedEl.value = state.start_seed;

      const numSeedsEl = document.getElementById("num_seeds");
      if (numSeedsEl && state.num_seeds != null)
        numSeedsEl.value = state.num_seeds;

      const gpuSelect = form.querySelector('[name="preferred_gpu"]');
      if (gpuSelect && state.preferred_gpu != null) {
        const opt = [...gpuSelect.options].find(
          (o) => o.value === state.preferred_gpu,
        );
        if (opt) gpuSelect.value = state.preferred_gpu;
      }

      if (Array.isArray(state.axes)) {
        state.axes.forEach((ax) => {
          addAxis(ax.name || "", (ax.values || []).join("\n"));
        });
      }
    } catch (e) {
      console.warn("Failed to restore batch form state:", e);
    }
  }

  updatePreview();
});

// ─── Viz form (new visualization page) ───────────────────────────────────────
onReady(() => {
  const form = document.getElementById("viz-form");
  if (!form) return;

  let nextVizIdx = 0;
  let vizPreviewExpanded = false;

  function sanitizeAxisValue(s) {
    return s
      .trim()
      .split(/\s+/)
      .join("_")
      .replace(/[^a-zA-Z0-9\-\_\.]+/g, "")
      .replace(/_+/g, "_")
      .replace(/^_|_$/g, "");
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
    const versionParts = combo.map(({ val }) => sanitizeAxisValue(val));
    const version =
      versionParts.length > 0 ? versionParts.join("_") : "default";
    let cmd = cmdTpl;
    cmd = cmd.replaceAll("{version}", version);
    combo.forEach(({ axis, val }) => {
      cmd = cmd.replaceAll("{" + axis.name + "}", sanitizeAxisValue(val));
    });
    return cmd;
  }

  function updateVizPreview() {
    const cmdTpl =
      (document.getElementById("viz_command_input") || {}).value || "";
    const axes = getVizAxes();
    const toggleable = axes;

    const el = document.getElementById("viz_preview_commands");
    const countEl = document.getElementById("viz_preview_count");
    const toggleBtn = document.getElementById("viz-preview-toggle");

    if (!cmdTpl.trim()) {
      if (el)
        el.innerHTML =
          '<div class="text-base-400">Enter a base command to preview.</div>';
      if (countEl) countEl.textContent = "";
      if (toggleBtn) toggleBtn.style.display = "none";
      const hidden = document.getElementById("axes_json");
      if (hidden) hidden.value = JSON.stringify(axes);
      return;
    }

    // Read the four routing fields.
    const inputArg =
      (form.querySelector('[name="input_argument"]') || {}).value || "";
    const inputPath =
      (form.querySelector('[name="input_path"]') || {}).value || "";
    const outputArg =
      (form.querySelector('[name="output_argument"]') || {}).value || "";
    const outputFile =
      (document.getElementById("viz_output_file") || {}).value || "";
    const dataPath = form.dataset.dataPath || "";

    // Build a suffix that will be appended to cmdTpl before resolveCmd runs,
    // so {version} and {axis_name} inside inputPath/outputFile get substituted.
    let suffix = "";
    if (inputArg && inputPath) {
      const resolvedInput = dataPath ? dataPath + "/" + inputPath : inputPath;
      suffix += " " + inputArg + " " + resolvedInput;
    }
    if (outputArg && outputFile) {
      const resolvedOutputTpl = dataPath
        ? dataPath + "/" + outputFile
        : outputFile;
      suffix += " " + outputArg + " " + resolvedOutputTpl;
    }

    const fullCmdTpl = cmdTpl + suffix;
    const hasSvg = fullCmdTpl.includes(".svg");
    const hasPng = fullCmdTpl.includes(".png");
    const hasDual = hasSvg || hasPng;

    const comboCount =
      toggleable.reduce((acc, ax) => acc * (ax.values.length || 1), 1) || 1;
    const totalCount = hasDual ? comboCount * 2 : comboCount;

    if (countEl)
      countEl.textContent =
        totalCount + " output" + (totalCount !== 1 ? "s" : "");

    const lineCount = hasDual ? comboCount * 2 : comboCount;
    if (toggleBtn) {
      toggleBtn.style.display = lineCount <= 5 ? "none" : "";
      toggleBtn.textContent = vizPreviewExpanded ? "Show less" : "Show all";
    }

    const combos = cartesianViz(
      toggleable.map((ax) => ax.values.map((v) => ({ axis: ax, val: v }))),
    );

    const allLines = combos.map((combo, i) => {
      const resolvedCmd = resolveCmd(fullCmdTpl, combo);
      const axisArgs = combo.map(({ val }) => val).filter(Boolean);
      const fullCmd =
        axisArgs.length > 0
          ? resolvedCmd + " " + axisArgs.join(" ")
          : resolvedCmd;

      const sep =
        i > 0 ? '<div class="border-t border-base-200 my-1.5"></div>' : "";

      if (hasSvg) {
        const pngCmd = fullCmd.replaceAll(".svg", ".png");
        return (
          sep +
          '<div class="truncate">' +
          esc(fullCmd) +
          "</div>" +
          '<div class="truncate text-base-400">' +
          esc(pngCmd) +
          "</div>"
        );
      } else if (hasPng) {
        const svgCmd = fullCmd.replaceAll(".png", ".svg");
        return (
          sep +
          '<div class="truncate">' +
          esc(svgCmd) +
          "</div>" +
          '<div class="truncate text-base-400">' +
          esc(fullCmd) +
          "</div>"
        );
      } else {
        return sep + '<div class="truncate">' + esc(fullCmd) + "</div>";
      }
    });

    const maxCombos = hasDual ? 2 : 5;
    if (el) {
      allVizLines = allLines.map((l) => {
        const tmp = document.createElement("div");
        tmp.innerHTML = l;
        return [...tmp.querySelectorAll("div")]
          .map((d) => d.textContent.trim())
          .filter(Boolean)
          .join("\n");
      });

      if (vizPreviewExpanded) {
        el.innerHTML = allLines.join("");
      } else {
        const visible = allLines.slice(0, maxCombos).join("");
        const hidden =
          comboCount > maxCombos
            ? '<div class="text-base-400 mt-1">… and ' +
            (comboCount - maxCombos) +
            ' more — click "Show all" to expand</div>'
            : "";
        el.innerHTML = visible + hidden;
      }
    }

    const hidden = document.getElementById("axes_json");
    if (hidden) hidden.value = JSON.stringify(axes);
  }

  window.toggleVizPreview = function() {
    vizPreviewExpanded = !vizPreviewExpanded;
    updateVizPreview();
  };

  function addVizAxis(initialName = "", initialValues = "") {
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
      placeholder="antmaze-large-play-v2\nantmaze-large-diverse-v2"
      class="w-full bg-transparent text-sm font-mono px-3 py-2.5 focus:outline-none resize-y placeholder:text-base-400"></textarea>
  `;

    const nameInput = div.querySelector(`#viz_axis_${idx}_name`);
    const valuesTextarea = div.querySelector(`#viz_axis_${idx}_values`);

    nameInput.addEventListener("input", updateVizPreview);
    valuesTextarea.addEventListener("input", updateVizPreview);
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

    if (initialName) nameInput.value = initialName;
    if (initialValues) valuesTextarea.value = initialValues;

    container.appendChild(div);
    updateVizPreview();
  }

  document
    .getElementById("viz_command_input")
    ?.addEventListener("input", updateVizPreview);
  document
    .getElementById("viz_output_file")
    ?.addEventListener("input", updateVizPreview);
  form
    .querySelector('[name="input_argument"]')
    ?.addEventListener("input", updateVizPreview);
  form
    .querySelector('[name="input_path"]')
    ?.addEventListener("input", updateVizPreview);
  form
    .querySelector('[name="output_argument"]')
    ?.addEventListener("input", updateVizPreview);

  form.addEventListener("submit", () => {
    const hidden = document.getElementById("axes_json");
    if (hidden) hidden.value = JSON.stringify(getVizAxes());
  });

  window.addVizAxis = addVizAxis;

  // ─── Redo: restore VizFormState ──────────────────────────────────────────
  const stateJSON = form.dataset.formState;
  if (stateJSON) {
    try {
      const state = JSON.parse(stateJSON);
      const setVal = (selector, val) => {
        if (val == null) return;
        const el = form.querySelector(selector);
        if (el) el.value = val;
      };

      setVal('[name="name"]', state.name);
      setVal('[name="description"]', state.description);
      setVal('[name="input_argument"]', state.input_argument);
      setVal('[name="input_path"]', state.input_path);
      setVal('[name="output_argument"]', state.output_argument);
      setVal('[name="output_file"]', state.output_file);

      const vizCmd = document.getElementById("viz_command_input");
      if (vizCmd && state.viz_command != null) vizCmd.value = state.viz_command;

      if (Array.isArray(state.axes)) {
        state.axes.forEach((ax) => {
          addVizAxis(ax.name || "", (ax.values || []).join("\n"));
        });
      }
    } catch (e) {
      console.warn("Failed to restore viz form state:", e);
    }
  }

  updateVizPreview();
});

onReady(() => {
  const viewer = document.getElementById("viz-viewer");
  if (!viewer) return;

  const vizId = viewer.dataset.vizId;
  const axes = JSON.parse(viewer.dataset.axes || "[]");
  const fileBaseUrl = viewer.dataset.fileUrl;

  const controls = document.getElementById("viz-controls");
  const img = document.getElementById("viz-output-img");
  const placeholder = document.getElementById("viz-output-placeholder");
  const spinner = document.getElementById("viz-output-spinner");
  const errorEl = document.getElementById("viz-output-error");
  const errorMsg = document.getElementById("viz-output-error-msg");
  const downloadSvg = document.getElementById("viz-download-svg");
  const downloadPng = document.getElementById("viz-download-png");
  const copyBtn = document.getElementById("viz-copy-png");

  // Combos dont le fichier est connu disponible (peuplé par les fragments WS).
  const availableCombos = new Set();

  // Sélection courante : première valeur de chaque axe par défaut.
  // Use axis INDEX as key (not name) to avoid collisions when names are empty.
  const selection = {};
  axes.forEach((ax, idx) => {
    if (ax.values && ax.values.length > 0) selection[idx] = ax.values[0];
  });

  // ── Calcul du comboKey JS (doit correspondre à la logique Go) ────────────
  // Go : indices séparés par "-", dans l'ordre des axes.
  function currentComboKey() {
    if (axes.length === 0) return "default";
    return axes
      .map((ax, idx) => {
        const selectedValue = selection[idx];
        const valueIdx = ax.values.indexOf(selectedValue);
        return String(valueIdx >= 0 ? valueIdx : 0);
      })
      .join("-");
  }

  // ── Construction des contrôles d'axes ────────────────────────────────────
  if (axes.length > 0) {
    controls.classList.remove("hidden");
    axes.forEach((ax, axisIdx) => {
      const row = document.createElement("div");
      row.className = "flex items-center justify-center gap-2 flex-wrap";

      if (ax.name) {
        const lbl = document.createElement("span");
        lbl.className =
          "text-xs font-semibold text-base-500 w-24 shrink-0 text-right";
        lbl.textContent = ax.name;
        row.appendChild(lbl);
      }

      ax.values.forEach((val) => {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.dataset.axisIndex = axisIdx;  // Use index instead of name
        btn.dataset.value = val;
        btn.textContent = val;
        btn.addEventListener("click", () => {
          selection[axisIdx] = val;  // Use index as key
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
    controls.querySelectorAll("button[data-axis-index]").forEach((btn) => {
      const axisIdx = btn.dataset.axisIndex;
      const active = selection[axisIdx] === btn.dataset.value;
      btn.className = active
        ? "text-xs px-2.5 py-1 rounded border bg-accent-500 text-white border-accent-600 transition-colors"
        : "text-xs px-2.5 py-1 rounded border bg-base-50 text-base-600 border-base-300 hover:bg-base-100 transition-colors";
    });
  }

  function buildUrl() {
    const params = new URLSearchParams();
    // Build query params: use axis name if it exists, otherwise use "axis{idx}"
    axes.forEach((ax, idx) => {
      if (selection[idx] !== undefined) {
        const paramName = ax.name || `axis${idx}`;
        params.set(paramName, selection[idx]);
      }
    });
    const qs = params.toString();
    return fileBaseUrl + (qs ? "?" + qs : "");
  }

  // ── États visuels ─────────────────────────────────────────────────────────

  function showSpinner() {
    img.classList.add("hidden");
    placeholder.classList.add("hidden");
    if (errorEl) errorEl.classList.add("hidden");
    if (spinner) spinner.classList.remove("hidden");
    if (downloadSvg) downloadSvg.classList.add("hidden");
    if (downloadPng) downloadPng.classList.add("hidden");
    if (copyBtn) copyBtn.classList.add("hidden");
  }

  function showPlaceholder() {
    img.classList.add("hidden");
    if (spinner) spinner.classList.add("hidden");
    if (errorEl) errorEl.classList.add("hidden");
    placeholder.classList.remove("hidden");
    if (downloadSvg) downloadSvg.classList.add("hidden");
    if (downloadPng) downloadPng.classList.add("hidden");
    if (copyBtn) copyBtn.classList.add("hidden");
  }

  function showError(msg) {
    img.classList.add("hidden");
    if (spinner) spinner.classList.add("hidden");
    placeholder.classList.add("hidden");
    if (errorEl) errorEl.classList.remove("hidden");
    if (errorMsg) errorMsg.textContent = msg;
    if (downloadSvg) downloadSvg.classList.add("hidden");
    if (downloadPng) downloadPng.classList.add("hidden");
    if (copyBtn) copyBtn.classList.add("hidden");
  }

  function showImage(url) {
    if (spinner) spinner.classList.add("hidden");
    placeholder.classList.add("hidden");
    if (errorEl) errorEl.classList.add("hidden");

    const svgUrl = url;
    const pngUrl = url + (url.includes("?") ? "&" : "?") + "format=png";

    img.src = svgUrl + (svgUrl.includes("?") ? "&" : "?") + "_t=" + Date.now();
    img.onload = () => img.classList.remove("hidden");
    img.onerror = () => showError("Failed to load output file.");

    if (downloadSvg) {
      downloadSvg.href = svgUrl;
      downloadSvg.classList.remove("hidden");
    }
    if (downloadPng) {
      downloadPng.href = pngUrl;
      downloadPng.classList.remove("hidden");
    }
    if (copyBtn) {
      copyBtn.classList.remove("hidden");
      copyBtn.onclick = async () => {
        try {
          const resp = await fetch(pngUrl);
          if (!resp.ok) throw new Error("PNG not available");
          const blob = await resp.blob();
          await navigator.clipboard.write([
            new ClipboardItem({ "image/png": blob }),
          ]);
          const prev = copyBtn.innerHTML;
          copyBtn.innerHTML = "Copied!";
          setTimeout(() => {
            copyBtn.innerHTML = prev;
          }, 2000);
        } catch {
          const prev = copyBtn.innerHTML;
          copyBtn.innerHTML = "Failed";
          setTimeout(() => {
            copyBtn.innerHTML = prev;
          }, 2000);
        }
      };
    }
  }

  // ── Chargement de l'image courante ────────────────────────────────────────

  function loadOutput() {
    const key = currentComboKey();
    const url = buildUrl();

    // Si ce combo est connu comme disponible, on tente directement.
    // Sinon on fait un HEAD pour vérifier.
    if (availableCombos.has(key)) {
      showImage(url);
      return;
    }

    fetch(url, { method: "HEAD" })
      .then((res) => {
        if (!res.ok) {
          showPlaceholder();
          return;
        }
        availableCombos.add(key);
        showImage(url);
      })
      .catch(() => showPlaceholder());
  }

  // ── Réception des événements WebSocket via MutationObserver ───────────────
  //
  // htmx-ext-ws fait un OOB swap sur #viz-result-{vizID} à chaque EventVizDone.
  // On observe les changements d'attributs de cet élément pour réagir.
  function processVizResult(node) {
    const comboKey = node.dataset.comboKey || "";
    const vizErr = node.dataset.error || "";
    const isCurrent = comboKey === currentComboKey();

    if (vizErr === "generating") {
      if (isCurrent) showSpinner();
      return;
    }

    if (vizErr !== "") {
      if (isCurrent) showError(vizErr);
      return;
    }

    availableCombos.add(comboKey);
    if (isCurrent) {
      showImage(buildUrl());
    }
  }

  const resultEl = document.getElementById("viz-result-" + vizId);
  if (resultEl && resultEl.parentNode) {
    const observer = new MutationObserver((mutations) => {
      for (const mutation of mutations) {
        for (const node of mutation.addedNodes) {
          if (
            node.nodeType === Node.ELEMENT_NODE &&
            node.id === "viz-result-" + vizId
          ) {
            processVizResult(node);
          }
        }
      }
    });
    observer.observe(resultEl.parentNode, { childList: true });
  }

  // ── Initialisation ────────────────────────────────────────────────────────

  updateButtons();

  // Si ?generating=true était dans l'URL, le spinner est déjà visible côté
  // HTML (data-generating="true"). On l'affiche immédiatement ici aussi pour
  // le cas où le snapshot WebSocket arrive avant le DOMContentLoaded.
  if (viewer.dataset.generating === "true") {
    showSpinner();
  } else {
    loadOutput();
  }
});

window.copyBatchPreview = function() {
  copyToClipboard(allBatchLines.join("\n"), "batch-copy-all");
};

window.copyVizPreview = function() {
  copyToClipboard(allVizLines.join("\n"), "viz-copy-all");
};

function copyToClipboard(text, btnId) {
  const btn = document.getElementById(btnId);
  if (!btn) return;
  const prev = btn.innerHTML;
  navigator.clipboard
    .writeText(text)
    .then(() => {
      btn.innerHTML = "Copied!";
      setTimeout(() => {
        btn.innerHTML = prev;
      }, 2000);
    })
    .catch(() => {
      btn.innerHTML = "Failed";
      setTimeout(() => {
        btn.innerHTML = prev;
      }, 2000);
    });
}

// ─── Inline retry command edit (job detail page) ──────────────────────────────

window.startEditRetry = function() {
  document.getElementById("retry-display").classList.add("hidden");
  const form = document.getElementById("retry-form");
  form.classList.remove("hidden");
  form.classList.add("flex");
  const input = document.getElementById("retry-input");
  input.focus();
  input.setSelectionRange(input.value.length, input.value.length);
};

window.cancelEditRetry = function() {
  document.getElementById("retry-form").classList.add("hidden");
  document.getElementById("retry-form").classList.remove("flex");
  document.getElementById("retry-display").classList.remove("hidden");
};

onReady(() => {
  const input = document.getElementById("retry-input");
  if (!input) return;
  input.addEventListener("keydown", (e) => {
    if (e.key === "Escape") cancelEditRetry();
  });
});

function syncDescription(form) {
    const div = form.querySelector('[contenteditable]');
    form.querySelector('[name="description"]').value = div.innerText;
}
