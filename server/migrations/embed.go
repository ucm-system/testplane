// Package migrations 只做一件事：把 migrations/*.sql 编译进二进制（05 §2.2 的
// `server/migrations`，golang-migrate 的目录约定）。
//
// 放独立包是为了 `//go:embed` 不能跨目录：store 包在 internal/store 下，
// 无法 embed 到 ../../migrations，所以由本包持有 FS，store 只负责执行与记账。
// 迁移文件只前向、只加不删（03 §7、05 §8）；文件名前缀是版本号。
package migrations

import "embed"

// FS 是全部迁移 SQL，按文件名排序即执行顺序。
//
//go:embed *.sql
var FS embed.FS
