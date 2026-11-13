"""宿主上下文替身：在一个"终端会话"里驱动插件包的 CLI 包装（plugins/hermes/tools/cli.py）。

用法：wrapper_driver.py <含 tools 目录的插件根> <会话引用> <apa 参数...>
替身提供 gateway.session_context.get_session_env（终端会话：平台、来源都为空），并把每次 apa 调用交给 APA_BIN
（测试里是一个记录调用环境后再转给真 apa 的脚本）。结果（exit_code、stdout、stderr）以 JSON 打印。
"""
import importlib.util
import json
import pathlib
import sys
import types

root, session, args = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3:]

gateway, ctx = types.ModuleType("gateway"), types.ModuleType("gateway.session_context")
ctx.get_session_env = lambda name, default="": default  # 终端会话：HERMES_SESSION_* 都未绑定
gateway.session_context = ctx
sys.modules.update({"gateway": gateway, "gateway.session_context": ctx})

spec = importlib.util.spec_from_file_location("apa_tools", root / "tools" / "__init__.py",
                                              submodule_search_locations=[str(root / "tools")])
mod = importlib.util.module_from_spec(spec)
sys.modules["apa_tools"] = mod
spec.loader.exec_module(mod)
print(json.dumps(mod.cli.call(args, session_id=session)))
