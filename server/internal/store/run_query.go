package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// 本文件是 Run 列表的**查询条件唯一来源**（D-61）。
//
// `List`（游标分页）、`ListPage`（offset 分页）、`CountRuns`、`Facets` 都从 `runConds.build`
// 取 WHERE，禁止各写一份——否则"page/total 与列表不一致""facet 计数与列表打架"迟早发生。

// runConds 是过滤条件的规范化形态（多值用切片）。
type runConds struct {
	statuses   []string
	kind       string
	submitters []string
	tester     string
}

// condsOf 把对外 RunFilter 转成条件；多值 submitter 走 IN。
func condsOf(f RunFilter) runConds {
	c := runConds{
		statuses:   f.Statuses,
		kind:       f.Kind,
		submitters: f.Submitters,
		tester:     f.Tester,
	}
	// 兼容旧字段：单值 Submitter 与多值合并（去重交给 SQL 的 = ANY）。
	if f.Submitter != "" {
		c.submitters = append(append([]string{}, c.submitters...), f.Submitter)
	}
	return c
}

// build 生成 WHERE 片段与参数；exclude 指定**不应用**哪个维度（facets 的标准语义：
// 算 status 的计数时应用其它维度过滤、不应用 status 自己）。
//
// exclude 取值：""（全部应用）/ "status" / "submitter" / "kind" / "tester"。
func (c runConds) build(exclude string) (string, []any) {
	var where []string
	var args []any
	add := func(expr string, vals ...any) {
		for _, v := range vals {
			args = append(args, v)
		}
		where = append(where, expr)
	}
	if exclude != "status" && len(c.statuses) > 0 {
		add(fmt.Sprintf("r.status = ANY($%d)", len(args)+1), c.statuses)
	}
	if exclude != "kind" && c.kind != "" {
		add(fmt.Sprintf("r.kind = $%d", len(args)+1), c.kind)
	}
	if exclude != "submitter" && len(c.submitters) > 0 {
		add(fmt.Sprintf("r.submitted_by = ANY($%d)", len(args)+1), c.submitters)
	}
	if exclude != "tester" && c.tester != "" {
		add(fmt.Sprintf(`EXISTS (SELECT 1 FROM tasks t WHERE t.run_id = r.id AND t.tester_name = $%d)`,
			len(args)+1), c.tester)
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// queryRuns 执行一条"取 Run 行"的查询（共享扫描逻辑）。
func (r *RunRepo) queryRuns(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]Run, error) {
	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

// ListPage 是 offset 分页（UI 用，D-61）：返回当前页与**满足过滤条件的总数**。
// page 从 1 开始；pageSize 已在 API 层规范化（1..100）。
func (r *RunRepo) ListPage(ctx context.Context, tx pgx.Tx, f RunFilter, page, pageSize int) ([]Run, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	where, args := condsOf(f).build("")
	// 总数与当页用同一 WHERE，但分两条语句，避免 count(*) OVER() 在空页时拿不到 total。
	var total int
	if err := r.s.db(tx).QueryRow(ctx,
		`SELECT count(*) FROM runs r`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count runs: %w", err)
	}
	q := `SELECT ` + runCols + ` FROM runs r` + where +
		fmt.Sprintf(" ORDER BY r.submitted_at DESC, r.id DESC LIMIT $%d OFFSET $%d",
			len(args)+1, len(args)+2)
	items, err := r.queryRuns(ctx, tx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// CountRuns 返回满足过滤条件的条数（与 ListPage 同一 WHERE）。
func (r *RunRepo) CountRuns(ctx context.Context, tx pgx.Tx, f RunFilter) (int, error) {
	where, args := condsOf(f).build("")
	var total int
	if err := r.s.db(tx).QueryRow(ctx,
		`SELECT count(*) FROM runs r`+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count runs: %w", err)
	}
	return total, nil
}

// FacetBucket 是列筛选的一个候选取值。
type FacetBucket struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// RunFacets 是四个维度的候选值（D-61 / 07 §5.1）。
type RunFacets struct {
	Status    []FacetBucket `json:"status"`
	Submitter []FacetBucket `json:"submitter"`
	Kind      []FacetBucket `json:"kind"`
	Tester    []FacetBucket `json:"tester"`
}

// Facets 计算四个维度的取值与计数。调用方应包在同一个事务里（同一快照），
// 否则计数与列表可能来自不同时刻。
//
// 标准 facet 语义：每个维度只应用**其它**维度的过滤（`build(dim)`），
// 这样"勾了 submitter=dev 之后各状态还有多少条"才是对的。
func (r *RunRepo) Facets(ctx context.Context, tx pgx.Tx, f RunFilter) (RunFacets, error) {
	c := condsOf(f)
	out := RunFacets{Status: []FacetBucket{}, Submitter: []FacetBucket{}, Kind: []FacetBucket{}, Tester: []FacetBucket{}}

	simple := func(exclude, column string) ([]FacetBucket, error) {
		where, args := c.build(exclude)
		q := `SELECT r.` + column + `, count(*) FROM runs r` + where +
			" GROUP BY 1 ORDER BY 2 DESC, 1 ASC"
		rows, err := r.s.db(tx).Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("store: facets %s: %w", column, err)
		}
		defer rows.Close()
		buckets := []FacetBucket{}
		for rows.Next() {
			var b FacetBucket
			if err := rows.Scan(&b.Value, &b.Count); err != nil {
				return nil, fmt.Errorf("store: scan facet %s: %w", column, err)
			}
			buckets = append(buckets, b)
		}
		return buckets, rows.Err()
	}

	var err error
	if out.Status, err = simple("status", "status"); err != nil {
		return RunFacets{}, err
	}
	if out.Submitter, err = simple("submitter", "submitted_by"); err != nil {
		return RunFacets{}, err
	}
	if out.Kind, err = simple("kind", "kind"); err != nil {
		return RunFacets{}, err
	}

	// tester 维度来自 tasks（一个 Run 可能命中多个 tester，按 Run 去重计数）。
	where, args := c.build("tester")
	q := `SELECT t.tester_name, count(DISTINCT r.id) FROM runs r
	        JOIN tasks t ON t.run_id = r.id` + where +
		" GROUP BY 1 ORDER BY 2 DESC, 1 ASC"
	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return RunFacets{}, fmt.Errorf("store: facets tester: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b FacetBucket
		if err := rows.Scan(&b.Value, &b.Count); err != nil {
			return RunFacets{}, fmt.Errorf("store: scan facet tester: %w", err)
		}
		out.Tester = append(out.Tester, b)
	}
	return out, rows.Err()
}
