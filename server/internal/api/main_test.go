package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/dbtest"
)

// TestMain 在包级跑测试前**重建 test 库的 public schema**。
//
// 为什么需要：`store.NewRunID` 的格式是 `r-YYYYMMDD-XXXX`（4 位随机，03 §3.1 固定），
// 单库累积几千条 Run 后单日随机段会开始撞 `runs_pkey`（概率随行数线性上升），
// 表现为随机失败（与业务无关）。清库后每次跑都是干净起点，测试才可重复。
//
// 只对名字含 `_test` 的库执行（dbtest.ResetSchema 内部再校验一次）。
func TestMain(m *testing.M) {
	if dsn, err := dbtest.URL("api"); err == nil && dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if _, rerr := dbtest.ResetSchema(ctx, dsn); rerr != nil {
			// 无法重置时仍继续（可能是没有 PG / 权限不足）：测试自身会 skip 或失败。
			os.Stderr.WriteString("api tests: 重置测试库失败（继续跑）: " + rerr.Error() + "\n")
		}
		cancel()
	}
	os.Exit(m.Run())
}
