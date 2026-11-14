import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  states: { pending: "待转发", forwarded: "已转发", processed: "已处理", handed_back: "已交回", released: "已退回判断流" } as Record<string, string>,
  title: "挂靠消息",
  none: "没有挂靠消息",
  list: "挂靠消息列表",
  sender: "发送者",
  unknownSender: "未知发送者",
  verifiedLabel: "核实状态",
  verified: "已核实",
  unverified: "未核实",
  allowLabel: "白名单",
  allowed: "已进白名单",
  notAllowed: "未进白名单",
  stateLabel: "处理状态",
  ownerOnly: " · 仅本人可见，不进入无人值守会话，不算已处理",
  source: "来源",
};

const en: typeof zh = {
  ...common.en,
  states: { pending: "to forward", forwarded: "forwarded", processed: "processed", handed_back: "handed back", released: "returned to judgment" },
  title: "Attached messages",
  none: "No attached messages",
  list: "Attached message list",
  sender: "Sender",
  unknownSender: "unknown sender",
  verifiedLabel: "Verification",
  verified: "verified",
  unverified: "unverified",
  allowLabel: "Allowlist",
  allowed: "on the allowlist",
  notAllowed: "not on the allowlist",
  stateLabel: "State",
  ownerOnly: " · visible only to you; not passed to unattended sessions and not counted as processed",
  source: "source",
};

export const strings: Dict<typeof zh> = { zh, en };
