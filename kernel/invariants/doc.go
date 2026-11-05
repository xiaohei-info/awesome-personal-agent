// Package invariants 是 §6.14 十条内核不变量的验收套件：每条一个测试，用真实模块装配出被约束的路径，
// 断言可观察的账本结果（Event、投影、退出码、投递）。套件本身没有生产代码；强杀实验在 tests/crash。
package invariants
