import { useSyncExternalStore } from "react";

// 「显示原始值」（设计 §4.2）：全局开关，存这台浏览器的 localStorage，默认关；打开后各处中文名旁显示内核 ID。
const KEY = "apa.raw";
const listeners = new Set<() => void>();

export const rawShown = () => typeof localStorage !== "undefined" && localStorage.getItem(KEY) === "1";

export function setRawShown(on: boolean) {
  if (on) localStorage.setItem(KEY, "1");
  else localStorage.removeItem(KEY);
  listeners.forEach((f) => f());
}

const subscribe = (f: () => void) => {
  listeners.add(f);
  return () => listeners.delete(f);
};

export function useRaw(): boolean {
  return useSyncExternalStore(subscribe, rawShown, rawShown);
}
