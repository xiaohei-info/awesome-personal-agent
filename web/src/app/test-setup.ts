import "@testing-library/jest-dom/vitest";
import { beforeEach } from "vitest";
import { setChoice } from "./i18n";

// jsdom 的浏览器语言是 en-US；现有用例按中文界面断言，每个用例前固定为中文，需要英文的用例自己切换。
beforeEach(() => setChoice("zh"));
