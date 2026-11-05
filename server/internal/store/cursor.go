package store

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 游标分页（07 §6、03 §5）：客户端只回传，不解析。
//
// 游标内容 = (submitted_at DESC, id DESC) 或 (seq) 或 (created_at, id)，base64url 编码。
// 服务端不暴露内部结构，但实现要能解回两个键做 keyset 比较。

// ErrBadCursor 表示游标解不开（应映射成 400 VALIDATION_ERROR）。
var ErrBadCursor = errors.New("bad cursor")

// EncodeCursor 把时间 + id 编码成不透明游标。
func EncodeCursor(ts time.Time, id string) string {
	raw := fmt.Sprintf("%d|%s", ts.UTC().UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 解出 (时间, id)。空串返回零值，调用方按"从头开始"处理。
func DecodeCursor(cur string) (time.Time, string, error) {
	if cur == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 不是合法 base64", ErrBadCursor)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("%w: 结构不正确", ErrBadCursor)
	}
	ns, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 时间戳非法", ErrBadCursor)
	}
	return time.Unix(0, ns).UTC(), parts[1], nil
}

// EncodeSeqCursor 用于按 seq 排序的列表（Run 内 Task 列表、事件回放）。
func EncodeSeqCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("s" + strconv.FormatInt(seq, 10)))
}

// DecodeSeqCursor 解出 seq；空串返回 0。
func DecodeSeqCursor(cur string) (int64, error) {
	if cur == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil || !strings.HasPrefix(string(raw), "s") {
		return 0, fmt.Errorf("%w: seq 游标非法", ErrBadCursor)
	}
	n, err := strconv.ParseInt(string(raw)[1:], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: seq 游标非法", ErrBadCursor)
	}
	return n, nil
}
