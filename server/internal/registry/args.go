package registry

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// PublicParams 是公共参数保留字（06 §5.4.1）。
var PublicParams = []string{domain.ParamURL, domain.ParamModelName, domain.ParamModelPath}

// BuildArgv 把提交者写的 args_raw 变成容器 argv（control-plane §C-8.4 + §C-8.5）：
//  1. 词法切分（domain.SplitArgs，手写扫描器，不走 shell）；
//  2. 公共参数冲突检查（提交者自己写了 --url 等 → 拒绝）；
//  3. 把有值的 --url/--model_name/--model_path 按固定顺序追加到末尾。
func BuildArgv(raw string, ep domain.Endpoint) ([]string, error) {
	argv, err := domain.SplitArgs(raw)
	if err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}
	if conflicts := domain.PublicParamConflict(argv); len(conflicts) > 0 {
		return nil, fmt.Errorf("args: public parameters must be injected by the platform, got %s: %w",
			strings.Join(conflicts, ", "), domain.ErrPrecondition)
	}
	return domain.InjectPublicParams(argv, ep), nil
}

// RequiredPublicParams 返回该 Tester 声明为 required 的公共参数名（按固定顺序）。
// 只有这三个公共参数参与 required 校验；其余 parameters 只供 Agent/人阅读（§5.4）。
func RequiredPublicParams(spec domain.TesterSpec) []string {
	required := make([]string, 0, len(PublicParams))
	for _, name := range PublicParams {
		if p, ok := spec.Parameters[name]; ok && p.Required {
			required = append(required, name)
		}
	}
	return required
}

// CheckPublicParams 在提交期校验 required 的公共参数都有值（C-5.1 第 11 条）。
// 返回缺失的参数名（空表示通过）。
func CheckPublicParams(spec domain.TesterSpec, ep domain.Endpoint) []string {
	var missing []string
	for _, name := range RequiredPublicParams(spec) {
		var value string
		switch name {
		case domain.ParamURL:
			value = ep.BaseURL
		case domain.ParamModelName:
			value = ep.ModelName
		case domain.ParamModelPath:
			value = ep.ModelPath
		}
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// EndpointKey 计算 endpoint_key（C-8.5 第 5 条）。解析失败时退回 raw:<sha256>，
// 保证同样的字符串仍然互相排斥。
func EndpointKey(raw string) string { return domain.NormalizeEndpointKey(raw) }
