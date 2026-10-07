#!/usr/bin/env python3
"""面板冒烟测试：用真实浏览器打开面板，断言登录框不可见、页签都能渲染。

为什么需要它：面板的「登录框永远显示」这个 bug 是**纯 CSS 层**的
（`.mask` 与 `.hidden` 同为单类选择器，`.mask` 写在后面 → `display:flex` 胜出），
Go 侧单测和 curl 全都看不出来 —— 只有真的开一个浏览器才发现。

依赖：pip install playwright && playwright install chromium
用法：python3 tools/panel-smoke.py [http://127.0.0.1:7868]
"""
import sys
from playwright.sync_api import sync_playwright

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:7868"

def main() -> int:
    failures = []
    with sync_playwright() as p:
        try:
            b = p.chromium.launch()
        except Exception:
            b = p.chromium.launch(channel="chrome")  # 用系统 Chrome 兜底
        pg = b.new_page(viewport={"width": 1280, "height": 1000})
        errs = []
        pg.on("pageerror", lambda e: errs.append(str(e)))
        pg.goto(BASE + "/?smoke=1", wait_until="networkidle")
        pg.wait_for_timeout(1200)

        login = pg.query_selector("#login")
        if login and login.is_visible():
            failures.append("登录框不该可见（服务端没设密码时它必须藏着）")

        header = pg.inner_text("header")
        for want in ("FutureSearch Gateway", "监听", "账号", "余额"):
            if want not in header:
                failures.append(f"页头缺 {want!r}：{header!r}")

        if len(pg.query_selector_all("#acctBody tr")) == 0:
            failures.append("账号表是空的（至少该有一行提示）")

        for tab in ("acct", "tenant", "models", "prompt", "usage", "set", "help"):
            pg.click(f'button[data-tab="{tab}"]')
            pg.wait_for_timeout(250)
            if not pg.is_visible(f"#tab-{tab}"):
                failures.append(f"页签 {tab} 点了不显示")

        if errs:
            failures.append(f"页面报错：{errs[:3]}")
        b.close()

    if failures:
        print("面板冒烟失败：")
        for f in failures:
            print("  ✗", f)
        return 1
    print("面板冒烟通过 ✓")
    return 0

if __name__ == "__main__":
    sys.exit(main())
