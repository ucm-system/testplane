package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"空串", "", []string{}},
		{"单参数", "--dataset=/data/a", []string{"--dataset=/data/a"}},
		{"连续空白", "a   b\t\tc\nd", []string{"a", "b", "c", "d"}},
		{"双引号含空格", `--dataset="/data/a b.json"`, []string{"--dataset=/data/a b.json"}},
		{"单引号含空格", `--name='a b'`, []string{"--name=a b"}},
		{"双引号转义", `--x="a\"b"`, []string{`--x=a"b`}},
		{"双引号反斜杠转义", `--x="a\\b"`, []string{`--x=a\b`}},
		{"双引号内不做变量展开", `--x="$HOME"`, []string{`--x=$HOME`}},
		{"双引号内反引号原样", "--x=`whoami`", []string{"--x=`whoami`"}},
		{"命令替换原样", `--x=$(whoami)`, []string{"--x=$(whoami)"}},
		{"分号原样", `--x=a;rm -rf /`, []string{"--x=a;rm", "-rf", "/"}},
		{"通配符原样", `--x=*`, []string{"--x=*"}},
		{"空参数合法", `"" ""`, []string{"", ""}},
		{"引号外反斜杠保留", `a\ b`, []string{`a\`, "b"}},
		{"未闭合单引号", `--x='a`, nil},
		{"未闭合双引号", `--x="a`, nil},
		{"NUL 字节", "a\x00b", nil},
		{"只有空白", "   \t\n ", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SplitArgs(c.raw)
			if c.want == nil {
				var le *ArgLexError
				if !errors.As(err, &le) {
					t.Fatalf("期望 ArgLexError，得到 err=%v argv=%v", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv = %#v, 期望 %#v", got, c.want)
			}
		})
	}
}

func TestSplitArgs_Limits(t *testing.T) {
	t.Run("argv 条数上限", func(t *testing.T) {
		raw := strings.Repeat("a ", MaxArgs+1)
		_, err := SplitArgs(raw)
		var le *ArgLexError
		if !errors.As(err, &le) || le.Code != IssueCodeArgsTooMany {
			t.Fatalf("期望 ARGS_TOO_MANY，得到 %v", err)
		}
	})

	t.Run("单参数上限", func(t *testing.T) {
		raw := strings.Repeat("x", MaxArgBytes+1)
		_, err := SplitArgs(raw)
		var le *ArgLexError
		if !errors.As(err, &le) || le.Code != IssueCodeArgTooLarge {
			t.Fatalf("期望 ARG_TOO_LARGE，得到 %v", err)
		}
	})

	t.Run("恰好等于上限合法", func(t *testing.T) {
		raw := strings.Repeat("x", MaxArgBytes)
		argv, err := SplitArgs(raw)
		if err != nil || len(argv) != 1 || len(argv[0]) != MaxArgBytes {
			t.Fatalf("err=%v len(argv)=%d", err, len(argv))
		}
	})
}

func TestPublicParamConflict(t *testing.T) {
	cases := []struct {
		argv []string
		want []string
	}{
		{[]string{"--dataset=/a"}, nil},
		{[]string{"--url=http://x"}, []string{"url"}},
		{[]string{"--url", "http://x"}, []string{"url"}},
		{[]string{"--model_name=m"}, []string{"model_name"}},
		{[]string{"--model_path=/m"}, []string{"model_path"}},
		{[]string{"--urlx=http://x"}, nil},     // 精确匹配，不走前缀误判
		{[]string{"--my-url=x"}, nil},
		{[]string{"--url=a", "--url=b"}, []string{"url"}}, // 只报一次
	}
	for _, c := range cases {
		if got := PublicParamConflict(c.argv); !reflect.DeepEqual(got, c.want) {
			t.Errorf("PublicParamConflict(%v) = %v, 期望 %v", c.argv, got, c.want)
		}
	}
}

func TestInjectPublicParams(t *testing.T) {
	ep := Endpoint{BaseURL: "http://h:8000/v1", ModelName: "Qwen3", ModelPath: ""}
	got := InjectPublicParams([]string{"--dataset=/a"}, ep)
	want := []string{"--dataset=/a", "--url=http://h:8000/v1", "--model_name=Qwen3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// 空值跳过
	got = InjectPublicParams(nil, Endpoint{BaseURL: "http://h"})
	want = []string{"--url=http://h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// 顺序固定 url → model_name → model_path
	got = InjectPublicParams(nil, Endpoint{BaseURL: "u", ModelName: "n", ModelPath: "p"})
	want = []string{"--url=u", "--model_name=n", "--model_path=p"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNormalizeEndpointKey(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"http://10.0.0.10:8000/v1", "http://10.0.0.10:8000"},
		{"HTTP://Example.COM:8000/v1", "http://example.com:8000"},
		{"10.0.0.10:8000", "http://10.0.0.10:8000"},
		{"example.com", "http://example.com:80"},
		{"https://example.com/v1", "https://example.com:443"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeEndpointKey(c.raw); got != c.want {
			t.Errorf("NormalizeEndpointKey(%q) = %q, 期望 %q", c.raw, got, c.want)
		}
	}

	// 解析失败时退化为 raw:<sha256>（仍保证同一字符串互斥）
	bad := "http://[::1"
	got := NormalizeEndpointKey(bad)
	if !strings.HasPrefix(got, "raw:") {
		t.Fatalf("解析失败应退化为 raw: 前缀，得到 %q", got)
	}
	if got != NormalizeEndpointKey(bad) {
		t.Fatal("同一字符串必须得到同一个 key")
	}
	if got == NormalizeEndpointKey(bad+"x") {
		t.Fatal("不同字符串必须得到不同 key")
	}
}
