package domain

import (
	"strings"
	"testing"
)

// C-8.4：输入校验（D-66/D-67）。jsonl 语法级 + 结构级；yaml/text 仅语法级。
func TestValidateJSONLInput(t *testing.T) {
	params := []Param{
		{Name: "dataset", Type: "string", Required: true},
		{Name: "input", Type: "integer"},
		{Name: "concurrency", Type: "number"},
		{Name: "tags", Type: "array"},
	}

	t.Run("合法多行", func(t *testing.T) {
		raw := "# 注释行\n" +
			`{"dataset":"/data/a","input":512,"concurrency":8,"tags":["x"]}` + "\n" +
			"\n" +
			`{"dataset":"/data/b","input":1024,"concurrency":8.5,"extra":"放行"}`
		issues := ValidateJSONLInput(raw, params)
		if len(issues) != 0 {
			t.Fatalf("issues = %v", issues)
		}
	})

	t.Run("非法 JSON 带行号", func(t *testing.T) {
		raw := "{\"dataset\":\"/a\"}\n{broken"
		issues := ValidateJSONLInput(raw, params)
		if len(issues) == 0 || issues[0].Line != 2 || issues[0].Code != IssueCodeInputSyntax {
			t.Fatalf("issues = %v, want line=2 INPUT_SYNTAX", issues)
		}
	})

	t.Run("行不是对象", func(t *testing.T) {
		issues := ValidateJSONLInput("[1,2]", params)
		if len(issues) == 0 || issues[0].Code != IssueCodeInputSyntax {
			t.Fatalf("issues = %v, want INPUT_SYNTAX", issues)
		}
	})

	t.Run("类型不匹配带字段名", func(t *testing.T) {
		raw := `{"dataset":123,"input":"512"}`
		issues := ValidateJSONLInput(raw, params)
		codes := map[string]bool{}
		for _, is := range issues {
			codes[is.Code+":"+is.Field] = true
		}
		if !codes[IssueCodeTypeMismatch+":dataset"] || !codes[IssueCodeTypeMismatch+":input"] {
			t.Fatalf("issues = %v", issues)
		}
	})

	t.Run("integer 接受 number 声明；number 不接受小数给 integer", func(t *testing.T) {
		p := []Param{{Name: "n", Type: "number"}, {Name: "i", Type: "integer"}}
		issues := ValidateJSONLInput(`{"n":8,"i":8}`, p)
		if len(issues) != 0 {
			t.Fatalf("issues = %v", issues)
		}
		issues = ValidateJSONLInput(`{"n":8,"i":8.5}`, p)
		if len(issues) != 1 || issues[0].Field != "i" {
			t.Fatalf("issues = %v", issues)
		}
	})

	t.Run("required 缺失", func(t *testing.T) {
		issues := ValidateJSONLInput(`{"input":1}`, params)
		found := false
		for _, is := range issues {
			if is.Code == IssueCodeRequired && is.Field == "dataset" {
				found = true
			}
		}
		if !found {
			t.Fatalf("issues = %v, want REQUIRED dataset", issues)
		}
	})

	t.Run("空输入缺 required", func(t *testing.T) {
		issues := ValidateJSONLInput("", params)
		if len(issues) == 0 || issues[0].Code != IssueCodeRequired {
			t.Fatalf("issues = %v", issues)
		}
	})

	t.Run("未声明字段放行", func(t *testing.T) {
		issues := ValidateJSONLInput(`{"dataset":"/a","unknown_key":true}`, params)
		for _, is := range issues {
			if is.Field == "unknown_key" {
				t.Fatalf("未声明字段应放行: %v", issues)
			}
		}
	})
}

func TestValidateYAMLInput(t *testing.T) {
	if issues := ValidateYAMLInput("model: Qwen3-32B\nreplicas: 1\n"); len(issues) != 0 {
		t.Fatalf("合法 yaml 被拒: %v", issues)
	}
	issues := ValidateYAMLInput("a: [unclosed")
	if len(issues) == 0 || issues[0].Code != IssueCodeInputSyntax {
		t.Fatalf("非法 yaml 应报 INPUT_SYNTAX: %v", issues)
	}
}

func TestValidateTextInput(t *testing.T) {
	if issues := ValidateTextInput("anything at all\n不校验"); len(issues) != 0 {
		t.Fatalf("text 输入应零校验: %v", issues)
	}
}

func TestInputFileName(t *testing.T) {
	cases := map[string]string{
		InputFormatJSONL: "params.jsonl",
		InputFormatYAML:  "params.yaml",
		InputFormatText:  "params.txt",
		"":               "params.jsonl",
	}
	for f, want := range cases {
		if got := InputFileName(f); got != want {
			t.Errorf("InputFileName(%q) = %q, want %q", f, got, want)
		}
	}
}

// C-8.5 第 5 条：endpoint_key 规范化。
func TestNormalizeEndpointKey(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"http://10.0.0.10:8000/v1", "http://10.0.0.10:8000"},
		{"HTTP://Example.COM", "http://example.com:80"},
		{"https://example.com", "https://example.com:443"},
		{"10.0.0.10:8000", "http://10.0.0.10:8000"},
	}
	for _, c := range cases {
		if got := NormalizeEndpointKey(c.raw); got != c.want {
			t.Errorf("NormalizeEndpointKey(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	k := NormalizeEndpointKey("::bad url::")
	if !strings.HasPrefix(k, "raw:") {
		t.Errorf("解析失败应退化为 raw:<sha256>，得到 %q", k)
	}
	if k != NormalizeEndpointKey("::bad url::") {
		t.Error("同一字符串的 raw 键必须互斥一致")
	}
}

func TestPublicParamsEnv(t *testing.T) {
	env := PublicParamsEnv(Endpoint{BaseURL: "http://x/v1", ModelName: "m"})
	if env[EnvServedURL] != "http://x/v1" || env[EnvServedModelName] != "m" {
		t.Fatalf("env = %v", env)
	}
	if _, ok := env[EnvModelPath]; ok {
		t.Error("空值不应注入")
	}
	for _, name := range PublicParamEnvs() {
		if !strings.HasPrefix(name, "AG_") {
			t.Errorf("公共参数 env 必须是 AG_* 前缀（D-66），得到 %q", name)
		}
	}
}

func TestReleaseName(t *testing.T) {
	n := ReleaseName("t-20261110-0001", 1)
	if !strings.HasPrefix(n, "arg-") || len(n) != 4+12 {
		t.Fatalf("release name = %q, want arg- + 12 hex（D-44）", n)
	}
	if n == ReleaseName("t-20261110-0001", 2) {
		t.Error("不同 attempt 必须派生不同 release（fresh deploy，§7.8）")
	}
}
