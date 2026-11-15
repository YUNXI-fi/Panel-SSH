# 应用包模板

复制本目录到 `apps/<slug>/`，然后：

1. 把 `app.json` 的 `slug` 改成文件夹名（小写字母、数字、连字符）。
2. 修改名称、分类、说明、动作与配置路径。
3. 替换 `icon.svg`（24×24 viewBox，`currentColor`）。
4. 按 `ui.json` 调整通用窗口的分组。
5. 在 `Panel SSH` 模块目录执行：

   ```bash
   go run ./cmd/appgen -out ../apps
   go run ./cmd/appgen -check -out ../apps
   ```

完整规范见 `docs/standards/application-adaptation.md`。
