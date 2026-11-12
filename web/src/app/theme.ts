// 明暗主题：默认跟随系统；本人手动切换后记在 localStorage，之后以它为准。
const KEY = "apa.theme";
const dark = () => window.matchMedia?.("(prefers-color-scheme: dark)");

const current = () => localStorage.getItem(KEY) ?? (dark()?.matches ? "dark" : "light");
const apply = () => {
  document.documentElement.dataset.theme = current();
};

export function followTheme() {
  apply();
  dark()?.addEventListener?.("change", apply);
}

export function toggleTheme() {
  localStorage.setItem(KEY, current() === "dark" ? "light" : "dark");
  apply();
}
