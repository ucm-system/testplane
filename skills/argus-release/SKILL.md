---
name: argus-release
description: Argus 发版流程——chart 版本三件套、tag 触发多架构镜像工作流、digest 回填与钉优先语义、GHCR 包可见性 internal 坑、GitHub Release 收尾。
---

# 发版流程（vX.Y.Z）

## 五步（沿 v0.1.2/#85、v0.2.0/#106 实证流程）

1. **发布 PR**（main 有分支保护，只走 squash-merge）：
   - `deploy/charts/argus/Chart.yaml`：`version` 与 `appVersion` 一起改（只改一个是历史事故源）；
   - `deploy/charts/argus/values.yaml`：主镜像 `tag` 改到新版本；**digest 不动**（还没有的值不能先写）；
   - `docs/runbook/release-X.Y.Z.md`：发布说明（镜像表 digest 留占位）；
   - 验证：`.tools/helm.exe lint deploy/charts/argus` + `helm template argus -f values-kind.yaml`（应渲染 `:kind-local`）。
2. **合并后打 tag**：`git tag vX.Y.Z origin/main && git push origin vX.Y.Z` →
   `.github/workflows/release.yml` 自动发 **7 镜像 × amd64/arm64**
   （server、web + `runners/*/build/Dockerfile` 自动发现的 5 个 runner；加 runner 不用改 workflow）。
3. **提取 digest**：run log 里 `steps.build.outputs.digest` 的 echo 行
   （`gh run view <id> --log | grep digest`），用
   `docker buildx imagetools inspect ghcr.io/ucm-system/argus-<name>:<tag>` 复核 index digest 一致。
4. **digest 回填 PR**（沿 #89/#107 模式）：`values.yaml` 钉 argus-server 的 index digest +
   发布说明镜像表补全 7 个 digest。
5. **GitHub Release**：`gh release create vX.Y.Z --notes-file …`，标题风格
   `vX.Y.Z · 主题一 / 主题二`。

## 语义与坑

- **digest 优先于 tag**（`_helpers.tpl`：digest 非空就用 digest）——回填前 release values
  部署的仍是上一个 digest，别在那个窗口用生产 values。
- **GHCR 包由 Actions 推送默认 internal（仅组织成员可拉）**，不会自动跟仓库公开；
  REST API 没有改可见性的端点（PATCH 404），要逐包在
  `github.com/orgs/ucm-system/packages/container/<name>/settings` → Danger Zone 手动改 Public，
  或在 org Settings → Packages → Package creation 改默认（只影响新包）。v0.2.0 实测翻过。
- **镜像名随 v2 更名是 `argus-*`**；改名/换代时旧 ghcr 路径停在最后版本不再更新——升级说明必须写。
- chart 版本没发过的号可以跳过（v0.1.3 从未发版，直接 0.1.3 → 0.2.0）；
  mock-llm 子 chart 的版本独立维护，别顺手改。
- tag 指向发布 PR 的 squash commit；后续 digest 回填 PR 不追改 tag（v0.1.2/v0.2.0 同款口径）。
