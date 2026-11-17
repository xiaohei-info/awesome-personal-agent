"""宿主上下文替身：按给定的 HERMES_SESSION_* 绑定，经插件包注册的原生工具运行一次调用。

用法：tool_driver.py <含 tools 目录的插件根> <会话引用> <绑定 JSON> <工具名|--list> [工具参数 JSON]
绑定 JSON 的键不带 HERMES_SESSION_ 前缀（platform、chat_type、user_id、source）；{} 表示什么都没绑定的终端。
--list 打印插件注册的工具：{名称: 描述}。调用时打印工具返回给模型的 JSON（exit_code、ok、stdout、stderr）。
APA_BIN 由调用方指向记录调用环境后转给真 apa 的脚本；APA_DIRECT_PLATFORMS 继承自调用方环境（本人声明的入口）。
"""
import importlib.util
import json
import pathlib
import sys
import types

root, session, bound, tool = pathlib.Path(sys.argv[1]), sys.argv[2], json.loads(sys.argv[3]), sys.argv[4]

gateway, ctx = types.ModuleType("gateway"), types.ModuleType("gateway.session_context")
ctx.get_session_env = lambda name, default="": bound.get(name.removeprefix("HERMES_SESSION_").lower(), default)
gateway.session_context = ctx
sys.modules.update({"gateway": gateway, "gateway.session_context": ctx})

spec = importlib.util.spec_from_file_location("apa_tools", root / "tools" / "__init__.py",
                                              submodule_search_locations=[str(root / "tools")])
mod = importlib.util.module_from_spec(spec)
sys.modules["apa_tools"] = mod
spec.loader.exec_module(mod)

registered = {}


class Host:
    def register_tool(self, name, handler, description, **_):
        registered[name] = (handler, description)


mod.register(Host())
if tool == "--list":
    print(json.dumps({n: d for n, (_, d) in registered.items()}))
else:
    print(registered[tool][0](json.loads(sys.argv[5]) if len(sys.argv) > 5 else {}, session_id=session))
