package auth

import "crypto/subtle"

// subtleEqual 是常量时间比较（CSRF token 不能提前返回）。
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
