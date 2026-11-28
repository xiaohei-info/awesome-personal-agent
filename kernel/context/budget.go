package kcontext

// InjectBudget 是面向 Agent 的一次注入或一次读取的字数上限（按字符计）：内核与命令行自己守住，不靠宿主截断。
// 与工作台概览（项目 L）同一个常量；该批合入后以 workbench.go 里的定义为准，本文件随之删除。
const InjectBudget = 4000
