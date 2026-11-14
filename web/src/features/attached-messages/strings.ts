import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "跟进消息",
  none: "还没有跟进消息",
  list: "跟进消息列表",
  sender: "发送者",
  unknownSender: "未知发送者",
  verifiedLabel: "核实状态",
  verified: "已核实",
  unverified: "未核实",
  allowLabel: "后台 agent 能否读到",
  allowed: "后台 agent 可读",
  notAllowed: "后台 agent 看不到",
  stateLabel: "处理状态",
  addToList: "加入可读名单",
  source: "来源",
};

const en: typeof zh = {
  ...common.en,
  title: "Follow-up messages",
  none: "No follow-up messages yet",
  list: "Follow-up message list",
  sender: "Sender",
  unknownSender: "unknown sender",
  verifiedLabel: "Verification",
  verified: "verified",
  unverified: "unverified",
  allowLabel: "Readable by background agents",
  allowed: "background agents can read it",
  notAllowed: "hidden from background agents",
  stateLabel: "State",
  addToList: "Add to the readable list",
  source: "source",
};

export const strings: Dict<typeof zh> = { zh, en };
