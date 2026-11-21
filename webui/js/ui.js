// DOM helpers, formatters, clipboard, toast and in-page dialogs.
import { t, dateLocale } from "./i18n.js";

export const $ = (selector, root = document) => root.querySelector(selector);
export const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

export function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value == null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key === "dataset") Object.assign(node.dataset, value);
    else if (key.startsWith("on") && typeof value === "function")
      node.addEventListener(key.slice(2), value);
    else if (value === true) node.setAttribute(key, "");
    else node.setAttribute(key, String(value));
  }
  for (const child of Array.isArray(children) ? children : [children]) {
    if (child == null || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

export const formatNumber = (value) => Number(value || 0).toLocaleString();

export function localTime(value) {
  return value ? new Date(value).toLocaleString(dateLocale(), { hour12: false }) : "—";
}

export function timeOnly(value) {
  return value ? new Date(value).toLocaleTimeString(dateLocale(), { hour12: false }) : "—";
}

let toastTimer = null;

export function toast(message, bad = false) {
  const node = $("#toast");
  node.textContent = message;
  node.classList.toggle("bad-toast", bad);
  node.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.classList.add("hidden"), 3600);
}

export async function copyText(text, okMessage = t("已复制", "toast.copied")) {
  try {
    await navigator.clipboard.writeText(text);
    toast(okMessage);
    return true;
  } catch {
    toast(t("浏览器不允许剪贴板访问，请手动选中复制", "toast.copyDenied"), true);
    return false;
  }
}

export function emptyRow(host, columns, message = t("暂无数据", "common.empty")) {
  host.append(el("tr", {}, [el("td", { class: "empty", colspan: columns, text: message })]));
}

export function renderFacts(host, entries) {
  host.replaceChildren();
  for (const [label, value, tone] of entries) {
    host.append(
      el("div", { class: "fact" }, [
        el("div", { class: "fact-label", text: label }),
        el("div", { class: `fact-value ${tone || ""}`.trim(), text: value ?? "—" }),
      ]),
    );
  }
}

const openDialogs = new Set();
const inertBackground = new Map();

function setDialogBackgroundInert() {
  if (inertBackground.size) return;
  const dialogHost = $("#dialog-host");
  for (const node of document.body.children) {
    if (node === dialogHost) continue;
    inertBackground.set(node, node.hasAttribute("inert"));
    node.setAttribute("inert", "");
  }
}

function restoreDialogBackground() {
  if (openDialogs.size) return;
  for (const [node, wasInert] of inertBackground) {
    if (wasInert) node.setAttribute("inert", "");
    else node.removeAttribute("inert");
  }
  inertBackground.clear();
}

// Generic dialog host; onClose fires on every close path so promise-based
// dialogs settle exactly once.
export function openDialog({ title, body = null, size = "", actions = [], onClose }) {
  const previous = document.activeElement;
  const dialogHost = $("#dialog-host");
  const previousDialogs = $$(".dialog", dialogHost).map((node) => [node, node.hasAttribute("inert")]);
  for (const [node] of previousDialogs) node.setAttribute("inert", "");
  setDialogBackgroundInert();
  const titleId = `dialog-title-${Math.random().toString(36).slice(2, 8)}`;
  const bodyId = `dialog-body-${Math.random().toString(36).slice(2, 8)}`;
  const overlay = el("div", { class: "dialog-overlay" });
  const dialog = el("div", {
    class: `dialog ${size}`.trim(),
    role: "dialog",
    "aria-modal": "true",
    "aria-labelledby": titleId,
    tabindex: "-1",
  });
  dialog.append(el("h2", { id: titleId, class: "dialog-title", text: title }));
  if (body) {
    if (body instanceof Element) {
      body.id ||= bodyId;
      dialog.setAttribute("aria-describedby", body.id);
    }
    dialog.append(body);
  }
  const focusable = () => [...dialog.querySelectorAll(
    'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  )].filter((node) => node.getClientRects().length > 0);
  const isTopmost = () => [...document.querySelectorAll(".dialog")].at(-1) === dialog;
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    openDialogs.delete(close);
    overlay.remove();
    for (const [node, wasInert] of previousDialogs) {
      if (wasInert) node.setAttribute("inert", "");
      else node.removeAttribute("inert");
    }
    document.removeEventListener("keydown", onKey, true);
    document.removeEventListener("focusin", onFocusIn, true);
    restoreDialogBackground();
    if (previous instanceof HTMLElement && previous.isConnected) previous.focus();
    onClose?.();
  };
  openDialogs.add(close);
  const onKey = (event) => {
    if (!isTopmost()) return;
    if (event.key === "Escape") {
      event.stopPropagation();
      close();
      return;
    }
    if (event.key !== "Tab") return;
    const nodes = focusable();
    if (!nodes.length) {
      event.preventDefault();
      dialog.focus();
      return;
    }
    const first = nodes[0];
    const last = nodes[nodes.length - 1];
    if (!dialog.contains(document.activeElement)) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
    } else if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };
  const onFocusIn = (event) => {
    if (!isTopmost() || dialog.contains(event.target)) return;
    const [first] = focusable();
    (first || dialog).focus();
  };
  if (actions.length) {
    const row = el("div", { class: "dialog-actions" });
    for (const action of actions) {
      row.append(
        el("button", {
          type: "button",
          class: `btn btn-sm ${action.kind || "btn-ghost"}`,
          text: action.label,
          onclick: () => (action.onClick ? action.onClick(close) : close()),
        }),
      );
    }
    dialog.append(row);
  }
  overlay.append(dialog);
  overlay.addEventListener("mousedown", (event) => {
    if (event.target === overlay) close();
  });
  document.addEventListener("keydown", onKey, true);
  document.addEventListener("focusin", onFocusIn, true);
  dialogHost.append(overlay);
  const target = dialog.querySelector("input, select, textarea") || dialog.querySelector(".dialog-actions .btn");
  (target || dialog).focus();
  return { close, dialog };
}

export function closeDialogs() {
  for (const close of [...openDialogs]) close();
}

export function confirmDialog({
  title,
  message = "",
  confirmLabel = t("确定", "common.confirm"),
  danger = false,
}) {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (value, close) => {
      if (!settled) {
        settled = true;
        resolve(value);
      }
      close?.();
    };
    openDialog({
      title,
      body: message ? el("p", { class: "dialog-text", text: message }) : null,
      onClose: () => finish(false),
      actions: [
        { label: t("取消", "common.cancel"), onClick: (close) => finish(false, close) },
        {
          label: confirmLabel,
          kind: danger ? "btn-danger" : "btn-primary",
          onClick: (close) => finish(true, close),
        },
      ],
    });
  });
}

export function promptDialog({
  title,
  label,
  value = "",
  placeholder = "",
  confirmLabel = t("确定", "common.confirm"),
}) {
  return new Promise((resolve) => {
    let settled = false;
    const input = el("input", { value, placeholder, maxlength: 80 });
    const finish = (result, close) => {
      if (!settled) {
        settled = true;
        resolve(result);
      }
      close?.();
    };
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        finish(input.value, dialog.close);
      }
    });
    const dialog = openDialog({
      title,
      body: el("div", { class: "field" }, [el("label", { text: label }), input]),
      onClose: () => finish(null),
      actions: [
        { label: t("取消", "common.cancel"), onClick: (close) => finish(null, close) },
        { label: confirmLabel, kind: "btn-primary", onClick: (close) => finish(input.value, close) },
      ],
    });
  });
}

// Tab strip wiring: [data-tab] buttons toggle [data-panel] panels and report
// the active id.
export function bindTabs(page, onSwitch) {
  const buttons = $$(".tabs .tab", page);
  const panels = $$("[data-panel]", page);
  const tabScope = page.id || "tabs";
  const slug = (value) => String(value).replace(/[^a-z0-9_-]+/gi, "-");
  for (const button of buttons) {
    const id = button.dataset.tab;
    const panel = panels.find((candidate) => candidate.dataset.panel === id);
    if (!panel) continue;
    const tabId = button.id || `${tabScope}-tab-${slug(id)}`;
    const panelId = panel.id || `${tabScope}-panel-${slug(id)}`;
    button.id = tabId;
    panel.id = panelId;
    button.type = "button";
    button.tabIndex = button.classList.contains("active") ? 0 : -1;
    button.setAttribute("aria-controls", panelId);
    panel.setAttribute("aria-labelledby", tabId);
    panel.setAttribute("aria-hidden", button.classList.contains("active") ? "false" : "true");
  }
  const activate = (id) => {
    for (const button of buttons) {
      const active = button.dataset.tab === id;
      button.classList.toggle("active", active);
      button.setAttribute("aria-selected", active ? "true" : "false");
      button.tabIndex = active ? 0 : -1;
      const panel = panels.find((candidate) => candidate.dataset.panel === button.dataset.tab);
      if (panel) panel.setAttribute("aria-hidden", active ? "false" : "true");
    }
    for (const panel of panels) panel.classList.toggle("hidden", panel.dataset.panel !== id);
    onSwitch?.(id);
  };
  for (const button of buttons) {
    button.addEventListener("click", () => activate(button.dataset.tab));
    button.addEventListener("keydown", (event) => {
      const index = buttons.indexOf(button);
      let nextIndex = index;
      if (event.key === "ArrowRight" || event.key === "ArrowDown") nextIndex = (index + 1) % buttons.length;
      else if (event.key === "ArrowLeft" || event.key === "ArrowUp") nextIndex = (index - 1 + buttons.length) % buttons.length;
      else if (event.key === "Home") nextIndex = 0;
      else if (event.key === "End") nextIndex = buttons.length - 1;
      else return;
      event.preventDefault();
      const next = buttons[nextIndex];
      activate(next.dataset.tab);
      next.focus();
    });
  }
  return { activate };
}
