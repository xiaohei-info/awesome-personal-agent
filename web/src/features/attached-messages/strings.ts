import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "跟进消息",
  none: "还没有跟进消息",
  openHint: "「未确认」不用你处理：挂错可交回；事项结束时重新判断。",
  list: "跟进消息列表",
  sender: "发送者",
  unknownSender: "未知发送者",
  verifiedLabel: "核实状态",
  verified: "已核实",
  unverified: "未核实",
  allowLabel: "Agent 后台运行能否读到",
  allowed: "Agent 后台运行能读",
  notAllowed: "Agent 后台运行读不到",
  stateLabel: "处理状态",
  addToList: "加入可读名单",
  source: "来源",
  trace: "处理经过",
};

const en: typeof zh = {
  ...common.en,
  title: "Follow-up messages",
  none: "No follow-up messages yet",
  openHint: "\"Unconfirmed\" needs nothing from you: it can be handed back; screened again when the item ends.",
  list: "Follow-up message list",
  sender: "Sender",
  unknownSender: "unknown sender",
  verifiedLabel: "Verification",
  verified: "verified",
  unverified: "unverified",
  allowLabel: "Readable by Agent background runs",
  allowed: "Agent background runs can read it",
  notAllowed: "hidden from Agent background runs",
  stateLabel: "State",
  addToList: "Add to the readable list",
  source: "source",
  trace: "what happened",
};

export const strings: Dict<typeof zh> = { zh, en };
