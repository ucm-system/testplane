-- 0002: runners.clean_runner —— deploy runner 声明式 clean 配对（D-70）
--
-- runner.yaml 的 `clean_runner: <clean runner 名>` 由 deploy runner 自己声明配对，
-- assemble 期解析（平台不感知具体名字）。此前该字段只在扫描内存里存在，
-- 库行与 API 都没带出去 —— 这里补列，让 GET /runners 能回显配对（提交页/目录页用）。
-- 空值 = 未声明 = 该 deploy runner 没有 clean 阶段（如 existing-deploy / mock-deploy）。

ALTER TABLE runners ADD COLUMN clean_runner text;
