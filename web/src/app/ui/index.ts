// 共用交互件（交互样式规格 §4、§5）。全部手写，不加依赖；样式在 ui.css。
import "./ui.css";

export * as Icon from "./icons";
export { Toaster, toast, toastError, undoToast, dismiss, resetToasts, undoLatest, typing } from "./toast";
export { Disclosure } from "./Disclosure";
export { PeekPanel, usePeek, useWide, rowEl } from "./Peek";
export { useListKeys, usePageKeys, usePageKeyList, KeyList, ShortcutSheet, LIST_KEYS, type KeyDef } from "./keys";
export { Menu, Popover, type MenuItem } from "./Menu";
export { ConfirmInline, ConfirmDialog } from "./Confirm";
export { SaveState, Skeleton, EmptyState, Clamp, type Save } from "./feedback";
export { collapse } from "./motion";
export { ErrorBoundary } from "./ErrorBoundary";
export { Highlight, terms } from "./Highlight";
export { useAutoSave } from "./autosave";
